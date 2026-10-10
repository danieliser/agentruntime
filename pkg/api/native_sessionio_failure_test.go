package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablememory "github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type failedClaudeFixtureAgent struct{ waitForInput bool }

func (*failedClaudeFixtureAgent) Name() string { return "claude" }
func (fixture *failedClaudeFixtureAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	script := `IFS= read -r prompt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"failed-fixture"}' '{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"fixture authentication failure"}'
`
	if fixture.waitForInput {
		script += "IFS= read -r another_prompt\n"
	}
	return []string{"/bin/sh", "-c", script}, nil
}
func (*failedClaudeFixtureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }

func TestV1ClaudeFailedResultFinalizesFailedWithoutWaitingForAnotherPrompt(t *testing.T) {
	for _, waits := range []bool{true, false} {
		name := "provider_exits_zero"
		if waits {
			name = "provider_waits_for_input"
		}
		t.Run(name, func(t *testing.T) {
			store := durablememory.New()
			registry := agent.NewRegistry()
			registry.Register(&failedClaudeFixtureAgent{waitForInput: waits})
			manager := session.NewManager()
			t.Cleanup(manager.ShutdownAll)
			server := NewServer(manager, runtime.NewLocalRuntime(), registry, ServerConfig{
				LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
			})
			httpServer := httptest.NewServer(server.router)
			defer httpServer.Close()
			response := postV1Session(t, httpServer.URL, map[string]any{
				"idempotency_key": name, "agent": "claude", "runtime": "local", "prompt": "fixture", "interactive": false,
			})
			defer response.Body.Close()
			var created struct {
				Data v1SessionData `json:"data"`
			}
			decodeJSON(t, response.Body, &created)
			if response.StatusCode != http.StatusCreated {
				t.Fatalf("create status=%d", response.StatusCode)
			}
			waitForDurableTerminal(t, store, created.Data.SessionID)
			receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
			if err != nil || receipt.State != durable.StateFailed || receipt.Reason != "failed" || receipt.ExitCode == nil || *receipt.ExitCode == 0 {
				t.Fatalf("expected failed receipt after provider failure: %+v error=%v", receipt, err)
			}
			waitForEventType(t, store, created.Data.SessionID, "turn.failed", 1)
		})
	}
}
