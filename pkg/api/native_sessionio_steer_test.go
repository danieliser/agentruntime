package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablememory "github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type steeredClaudeFixtureAgent struct {
	boundaryReason string
	finalFailed    bool
	finishPath     string
}

func (*steeredClaudeFixtureAgent) Name() string { return "claude" }
func (fixture *steeredClaudeFixtureAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	script := `IFS= read -r prompt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"steer-fixture"}'
IFS= read -r interrupt
IFS= read -r replacement
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"` + fixture.boundaryReason + `","permission_denials":[],"safety_stops":0}'
while [ ! -f "` + fixture.finishPath + `" ]; do sleep 0.01; done
`
	if fixture.finalFailed {
		script += `printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"api_error"}'`
	} else {
		script += `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"replacement finished"}'`
	}
	script += "\nIFS= read -r final_wait\n"
	return []string{"/bin/sh", "-c", script}, nil
}
func (*steeredClaudeFixtureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }

func TestV1ClaudeSteerDoesNotTerminateItsReplacement(t *testing.T) {
	for _, scenario := range []string{"success", "tools_abort", "replacement_failure", "real_error", "cancel", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			store := durablememory.New()
			registry := agent.NewRegistry()
			fixture := &steeredClaudeFixtureAgent{finishPath: filepath.Join(t.TempDir(), "finish"), boundaryReason: "aborted_streaming", finalFailed: scenario == "replacement_failure"}
			if scenario == "tools_abort" {
				fixture.boundaryReason = "aborted_tools"
			}
			if scenario == "real_error" {
				fixture.boundaryReason = "api_error"
			}
			registry.Register(fixture)
			manager := session.NewManager()
			t.Cleanup(manager.ShutdownAll)
			server := NewServer(manager, runtime.NewLocalRuntime(), registry, ServerConfig{LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
			httpServer := httptest.NewServer(server.router)
			defer httpServer.Close()
			body := map[string]any{"idempotency_key": scenario, "agent": "claude", "runtime": "local", "prompt": "original", "interactive": false}
			if scenario == "deadline" {
				body["timeout"] = "250ms"
			}
			response := postV1Session(t, httpServer.URL, body)
			defer response.Body.Close()
			var created struct {
				Data v1SessionData `json:"data"`
			}
			decodeJSON(t, response.Body, &created)
			if response.StatusCode != http.StatusCreated {
				t.Fatalf("create status=%d", response.StatusCode)
			}
			id := created.Data.SessionID
			waitForEventType(t, store, id, "lifecycle.provider.initialized", 1)
			liveSession := manager.Get(id)
			if liveSession == nil {
				t.Fatal("missing session")
			}
			resultWait := liveSession.ResultCh()
			if err := server.SendSessionInput(context.Background(), id, "steer", "steer", "replacement"); err != nil {
				t.Fatal(err)
			}
			if scenario != "real_error" {
				waitForEventType(t, store, id, "turn.superseded", 1)
				select {
				case <-resultWait:
					t.Fatal("superseded turn notified completion watchers")
				default:
				}
				if receipt, err := store.GetTerminalReceipt(context.Background(), id); err == nil {
					t.Fatalf("interrupted turn killed replacement: %+v", receipt)
				}
				if scenario == "cancel" {
					cancel := requestJSON(t, http.MethodPost, httpServer.URL+"/api/v1/sessions/"+id+"/cancel", map[string]any{"idempotency_key": "cancel"})
					defer cancel.Body.Close()
					if cancel.StatusCode != http.StatusAccepted && cancel.StatusCode != http.StatusOK {
						t.Fatalf("cancel status=%d", cancel.StatusCode)
					}
				} else if scenario != "deadline" {
					// The replacement is still active: let the fixture finish its turn
					// without queueing a new prompt into that active turn.
					if err := os.WriteFile(fixture.finishPath, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			waitForDurableTerminal(t, store, id)
			receipt, err := store.GetTerminalReceipt(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			expected := durable.StateCompleted
			if scenario == "real_error" || scenario == "replacement_failure" {
				expected = durable.StateFailed
			}
			if scenario == "cancel" {
				expected = durable.StateCancelled
			}
			if scenario == "deadline" {
				expected = durable.StateTimedOut
			}
			if receipt.State != expected {
				t.Fatalf("state=%s want=%s receipt=%+v", receipt.State, expected, receipt)
			}
		})
	}
}
