package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// Providers are deterministic wire fixtures; runtimes are a process boundary
// fixture so this matrix never invokes a paid provider or a Docker daemon.
func TestV1PromptWakesCompletedGeneration(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, runtimeName := range []string{"local", "docker"} {
			t.Run(provider+"/"+runtimeName, func(t *testing.T) {
				store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				fixture := &continuationAgent{provider: provider}
				registry := agent.NewRegistry()
				registry.Register(fixture)
				rt := &continuationRuntime{name: runtimeName}
				server := NewServer(session.NewManager(), rt, registry, ServerConfig{DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
				httpServer := httptest.NewServer(server.router)
				defer httpServer.Close()
				request := map[string]any{"idempotency_key": "wake-create", "agent": provider, "runtime": runtimeName, "prompt": "first", "model": "fixture-model", "effort": "low", "env": map[string]string{"WAKE_FIXTURE": "retained"}}
				if runtimeName == "docker" {
					request["work_dir"] = t.TempDir()
					request["provider_permissions"] = map[string]string{"mode": "restricted", "approval_policy": "on-request"}
				}
				response := postV1Session(t, httpServer.URL, request)
				var created struct {
					Data v1SessionData `json:"data"`
				}
				decodeJSON(t, response.Body, &created)
				response.Body.Close()
				if response.StatusCode != http.StatusCreated {
					t.Fatalf("create status=%d", response.StatusCode)
				}
				id := created.Data.SessionID
				waitForDurableTerminal(t, store, id)
				first, err := store.GetTerminalReceipt(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				for number := int64(2); number <= 3; number++ {
					view := get(t, httpServer, "/api/v1/sessions/"+id)
					var envelope struct {
						Data struct {
							Continuable bool   `json:"continuable"`
							Reason      string `json:"continuable_reason"`
						} `json:"data"`
					}
					decodeJSON(t, view.Body, &envelope)
					view.Body.Close()
					if !envelope.Data.Continuable || envelope.Data.Reason == "" {
						t.Errorf("completed session continuation=%+v", envelope.Data)
					}
					body := map[string]any{"idempotency_key": fmt.Sprintf("wake-%d", number), "kind": "prompt", "text": "follow-up"}
					var wg sync.WaitGroup
					statuses := make(chan int, 2)
					for range 2 {
						wg.Add(1)
						go func() {
							defer wg.Done()
							reply := postV1Control(t, httpServer.URL, id, "input", body)
							defer reply.Body.Close()
							statuses <- reply.StatusCode
						}()
					}
					wg.Wait()
					close(statuses)
					accepted, repeated := 0, 0
					for status := range statuses {
						if status == http.StatusAccepted {
							accepted++
						}
						if status == http.StatusOK {
							repeated++
						}
					}
					if accepted != 1 || repeated != 1 {
						t.Fatalf("wake statuses: accepted=%d repeated=%d", accepted, repeated)
					}
					waitForDurableTerminal(t, store, id)
					latest, err := store.GetTerminalReceipt(context.Background(), id)
					if err != nil || latest.Generation != number || latest.State != durable.StateCompleted || latest.LastSequence <= first.LastSequence {
						t.Fatalf("receipt=%+v err=%v", latest, err)
					}
					if first.OutputHash != outputHash(session.LogFilePath(server.logDir, id)) {
						t.Fatal("continuation changed the file covered by generation one's receipt")
					}
					if latest.OutputHash != outputHash(session.LogFilePath(server.logDir, fmt.Sprintf("%s.g%d", id, number))) {
						t.Fatal("receipt does not cover its generation's output")
					}

					conflict := postV1Control(t, httpServer.URL, id, "input", map[string]any{"idempotency_key": fmt.Sprintf("wake-%d", number), "kind": "prompt", "text": "changed"})
					conflict.Body.Close()
					if conflict.StatusCode != http.StatusConflict {
						t.Fatalf("changed retry status=%d", conflict.StatusCode)
					}
				}
				// A fresh server has no live session or in-memory control state.
				restarted := NewServer(session.NewManager(), rt, registry, ServerConfig{DataDir: server.dataDir, LogDir: server.logDir, DurableStore: store, EventBroker: eventstream.New(store)})
				restartedHTTP := httptest.NewServer(restarted.router)
				defer restartedHTTP.Close()
				retry := postV1Control(t, restartedHTTP.URL, id, "input", map[string]any{"idempotency_key": "wake-2", "kind": "prompt", "text": "follow-up"})
				retry.Body.Close()
				if retry.StatusCode != http.StatusOK {
					t.Fatalf("restart retry status=%d", retry.StatusCode)
				}
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				if len(fixture.configs) != 4 {
					t.Fatalf("build count=%d", len(fixture.configs))
				}
				// Admission resolves once before the initial spawn resolves again.
				for _, config := range fixture.configs[2:] {
					if config.ResumeSessionID != "fixture-thread" || config.Model != "fixture-model" || config.Effort != "low" {
						t.Fatalf("resume config=%+v", config)
					}
					if runtimeName == "docker" && config.PermissionMode != "default" {
						t.Fatal("resume widened provider permissions")
					}
				}
				rt.mu.Lock()
				defer rt.mu.Unlock()
				if len(rt.configs) != 3 {
					t.Fatalf("spawns=%d", len(rt.configs))
				}
				for _, config := range rt.configs[1:] {
					if config.Prompt != "follow-up" || config.Generation < 2 || config.Request.Runtime != runtimeName {
						t.Fatalf("wrong continuation spawn")
					}
					if runtimeName == "docker" && config.VolumeName != "agentruntime-vol-"+id {
						t.Fatalf("volume=%s", config.VolumeName)
					}
					if config.Env["WAKE_FIXTURE"] != "retained" || config.ExecutionPolicyHash != rt.configs[0].ExecutionPolicyHash {
						t.Fatal("resume lost manifest environment or execution policy hash")
					}
				}
				page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: id, Limit: 1000})
				if err != nil {
					t.Fatal(err)
				}
				for index, event := range page.Events {
					if event.Sequence != int64(index+1) {
						t.Fatalf("sequence gap at %d", index)
					}
				}
				if err := store.CheckIntegrity(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type continuationRuntime struct {
	name    string
	mu      sync.Mutex
	configs []runtime.SpawnConfig
}

func (rt *continuationRuntime) Name() string { return rt.name }
func (rt *continuationRuntime) Spawn(ctx context.Context, config runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	rt.mu.Lock()
	rt.configs = append(rt.configs, config)
	rt.mu.Unlock()
	return runtime.NewLocalRuntime().Spawn(ctx, config)
}
func (*continuationRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) {
	return nil, nil
}
func (*continuationRuntime) Cleanup(context.Context) error { return nil }

type continuationAgent struct {
	provider   string
	maintained bool
	mu         sync.Mutex
	configs    []agent.AgentConfig
}

func (a *continuationAgent) Name() string                                  { return a.provider }
func (a *continuationAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }
func (a *continuationAgent) BuildCmd(_ string, config agent.AgentConfig) ([]string, error) {
	a.mu.Lock()
	a.configs = append(a.configs, config)
	a.mu.Unlock()
	if a.provider == "claude" {
		return a.keepOpen([]string{"/bin/sh", "-c", `IFS= read -r prompt
case "$prompt" in *'"text":"first"'*|*'"text":"follow-up"'*) ;; *) exit 9 ;; esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"fixture-thread"}' '{"type":"result","subtype":"success","is_error":false,"result":"continued"}'`}), nil
	}
	method := "thread/start"
	if config.ResumeSessionID != "" {
		method = "thread/resume"
	}
	script := `IFS= read -r initialize
printf '%s\n' '{"id":0,"result":{"userAgent":"fixture"}}'
IFS= read -r initialized
IFS= read -r thread
case "$thread" in *'"method":"METHOD"'*) ;; *) exit 9 ;; esac
printf '%s\n' '{"id":1,"result":{"threadId":"fixture-thread"}}'
IFS= read -r prompt
case "$prompt" in *'"text":"first"'*|*'"text":"follow-up"'*) ;; *) exit 9 ;; esac
printf '%s\n' '{"id":2,"result":{}}' '{"method":"turn/started","params":{"threadId":"fixture-thread","turnId":"fixture-turn"}}' '{"method":"turn/completed","params":{"threadId":"fixture-thread","turnId":"fixture-turn","status":"completed"}}'`
	return a.keepOpen([]string{"/bin/sh", "-c", strings.ReplaceAll(script, "METHOD", method)}), nil
}

func (a *continuationAgent) keepOpen(command []string) []string {
	if a.maintained {
		command[2] += "\nIFS= read -r keepalive\n"
	}
	return command
}

func TestMaintainedLeaseExpiryWakesBothProviders(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			registry := agent.NewRegistry()
			registry.Register(&continuationAgent{provider: provider, maintained: true})
			rt := &continuationRuntime{name: "docker"}
			server := NewServer(session.NewManager(), rt, registry, ServerConfig{DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
			httpServer := httptest.NewServer(server.router)
			defer httpServer.Close()
			response := postV1Session(t, httpServer.URL, map[string]any{"idempotency_key": "lease-expiry", "agent": provider, "runtime": "docker", "prompt": "first", "container_lease": map[string]any{"mode": "maintain", "idle_ttl": "20ms"}})
			var created struct {
				Data v1SessionData `json:"data"`
			}
			decodeJSON(t, response.Body, &created)
			response.Body.Close()
			if response.StatusCode != http.StatusCreated {
				t.Fatalf("create status=%d", response.StatusCode)
			}
			id := created.Data.SessionID
			waitForDurableTerminal(t, store, id)
			wake := postV1Control(t, httpServer.URL, id, "input", map[string]any{"idempotency_key": "after-expiry", "kind": "prompt", "text": "follow-up"})
			wake.Body.Close()
			if wake.StatusCode != http.StatusAccepted {
				t.Fatalf("wake status=%d", wake.StatusCode)
			}
			waitForDurableTerminal(t, store, id)
			receipt, err := store.GetTerminalReceipt(context.Background(), id)
			if err != nil || receipt.Generation != 2 || receipt.State != durable.StateCompleted {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if err := store.CheckIntegrity(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
