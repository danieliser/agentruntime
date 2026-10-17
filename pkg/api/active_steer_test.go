package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/session"
)

// The Docker runtime boundary is a local subprocess fixture, as in the wake
// tests. Real Docker/provider acceptance is scripts/agentctl-live-test.sh.
func TestActiveDockerSteerAcrossContinuationModes(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, mode := range []string{"initial", "wake", "warm"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				store, err := sqlite.Open(filepath.Join(root, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				fixture := &activeSteerAgent{provider: provider, mode: mode, release: filepath.Join(root, "release")}
				registry := agent.NewRegistry()
				registry.Register(fixture)
				manager := session.NewManager()
				t.Cleanup(manager.ShutdownAll)
				rt := &continuationRuntime{name: "docker"}
				server := NewServer(manager, rt, registry, ServerConfig{DataDir: root, LogDir: filepath.Join(root, "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
				httpServer := httptest.NewServer(server.router)
				t.Cleanup(httpServer.Close)
				request := map[string]any{"idempotency_key": "create", "agent": provider, "runtime": "docker", "prompt": "original", "work_dir": root}
				if mode == "warm" {
					request["container_lease"] = map[string]string{"mode": "maintain", "idle_ttl": "1m"}
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
				input := func(key, kind, text string, want int) {
					t.Helper()
					r := postV1Control(t, httpServer.URL, id, "input", map[string]string{"idempotency_key": key, "kind": kind, "text": text})
					defer r.Body.Close()
					if r.StatusCode != want {
						t.Fatalf("%s status=%d want=%d", key, r.StatusCode, want)
					}
				}
				generation := int64(1)
				if mode != "initial" {
					waitForEventType(t, store, id, "turn.completed", 1)
					if mode == "wake" {
						waitForDurableTerminal(t, store, id)
						generation = 2
					} else {
						waitForSteerIdle(t, server, id)
					}
					input("followup", "prompt", "active", http.StatusAccepted)
				}
				waitForEventType(t, store, id, "tool.call", 1)
				input("busy", "prompt", "extra", http.StatusConflict)
				input("steer", "steer", "replacement", http.StatusAccepted)
				input("steer", "steer", "replacement", http.StatusOK)
				input("steer", "steer", "changed", http.StatusConflict)
				// The fixture emits this only after validating the native steer wire.
				waitForEventType(t, store, id, "tool.result", 1)
				if provider == "claude" {
					waitForEventType(t, store, id, "turn.superseded", 1)
				}
				input("still-busy", "prompt", "extra", http.StatusConflict)
				stored, err := store.GetSession(ctx, id)
				if err != nil || stored.State != durable.StateRunning || stored.ActiveGeneration != generation {
					t.Fatalf("steer changed lifecycle: state=%s generation=%d err=%v", stored.State, stored.ActiveGeneration, err)
				}
				if receipt, err := store.GetTerminalReceipt(ctx, id); err == nil && receipt.Generation == generation {
					t.Fatal("steer prematurely finalized the active generation")
				}
				if err := os.WriteFile(fixture.release, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				completed := 1
				if mode != "initial" {
					completed++
				}
				waitForEventType(t, store, id, "turn.completed", completed)
				if mode == "warm" {
					waitForSteerIdle(t, server, id)
				} else {
					waitForDurableTerminal(t, store, id)
					receipt, err := store.GetTerminalReceipt(ctx, id)
					if err != nil || receipt.State != durable.StateCompleted || receipt.Generation != generation {
						t.Fatalf("replacement receipt=%+v err=%v", receipt, err)
					}
				}
				if err := store.CheckIntegrity(ctx); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func waitForSteerIdle(t *testing.T, server *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if active := server.nativeSession(id); active != nil && active.lease.IsIdle() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("maintained replacement did not become idle")
}

type activeSteerAgent struct {
	provider string
	mode     string
	release  string
}

func (a *activeSteerAgent) Name() string                                { return a.provider }
func (*activeSteerAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }
func (a *activeSteerAgent) BuildCmd(_ string, config agent.AgentConfig) ([]string, error) {
	var script strings.Builder
	script.WriteString("set -eu\n")
	if a.provider == "codex" {
		script.WriteString(`IFS= read -r initialize
printf '%s\n' '{"id":0,"result":{"userAgent":"fixture"}}'
IFS= read -r initialized
IFS= read -r thread
printf '%s\n' '{"id":1,"result":{"threadId":"steer-thread"}}'
`)
	}
	script.WriteString("IFS= read -r prompt\n")
	if a.provider == "claude" {
		script.WriteString(`printf '%s\n' '{"type":"system","subtype":"init","session_id":"steer-thread"}'` + "\n")
	}
	if a.mode != "initial" && config.ResumeSessionID == "" {
		script.WriteString(a.completed())
		if a.mode == "wake" {
			script.WriteString("exit 0\n")
			return []string{"/bin/sh", "-c", script.String()}, nil
		}
		script.WriteString("IFS= read -r followup\n")
	}
	if a.provider == "claude" {
		script.WriteString(`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool","name":"Bash","input":{}}]}}'
IFS= read -r interrupt
[ "$interrupt" = '{"request":{"subtype":"interrupt"},"type":"control_request"}' ]
IFS= read -r steer
[ "$steer" = '{"message":{"content":[{"text":"replacement","type":"text"}],"role":"user"},"type":"user"}' ]
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_tools","permission_denials":[],"safety_stops":0}' '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool","content":"steer verified"}]}}'
`)
	} else {
		script.WriteString(`printf '%s\n' '{"method":"turn/started","params":{"threadId":"steer-thread","turn":{"id":"active-turn"}}}' '{"method":"item/started","params":{"threadId":"steer-thread","turnId":"active-turn","item":{"type":"commandExecution"}}}'
IFS= read -r steer
case "$steer" in *'"method":"turn/steer"'*'"expectedTurnId":"active-turn"'*'"text":"replacement"'*'"threadId":"steer-thread"'*) ;; *) exit 9 ;; esac
printf '%s\n' '{"method":"item/completed","params":{"threadId":"steer-thread","turnId":"active-turn","item":{"type":"commandExecution","output":"steer verified"}}}'
`)
	}
	fmt.Fprintf(&script, "while [ ! -f %q ]; do sleep 0.01; done\n", a.release)
	script.WriteString(a.completed())
	script.WriteString("IFS= read -r keepalive\n")
	return []string{"/bin/sh", "-c", script.String()}, nil
}

func (a *activeSteerAgent) completed() string {
	if a.provider == "claude" {
		return `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"finished"}'` + "\n"
	}
	return `printf '%s\n' '{"method":"turn/completed","params":{"threadId":"steer-thread","turn":{"id":"active-turn","status":"completed"}}}'` + "\n"
}
