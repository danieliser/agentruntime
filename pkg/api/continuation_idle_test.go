package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestPromptWakesLostIdleGenerationButRejectsUnfinishedTurn(t *testing.T) {
	for _, idle := range []bool{true, false} {
		t.Run(map[bool]string{true: "idle", false: "unfinished"}[idle], func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			seedContinuation(t, store, durable.StateCompleted, "fixture-thread")
			if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "boundary", From: durable.StateCompleted, To: durable.StateStarting}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "boundary", Runtime: "local", ContainerID: "idle-generation", ProviderID: "fixture-thread"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "boundary", From: durable.StateStarting, To: durable.StateRunning}); err != nil {
				t.Fatal(err)
			}
			if idle {
				if _, err := store.AppendEvent(ctx, durable.AppendEventParams{SessionID: "boundary", Generation: 2, EventID: "idle-turn", Timestamp: time.Now(), SchemaVersion: "1.0", Type: "turn.completed", Stream: durable.StreamProviderStdout, Raw: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{SessionID: "boundary", Generation: 2, From: durable.GenerationStarting, To: durable.GenerationLost}); err != nil {
				t.Fatal(err)
			}
			registry := agent.NewRegistry()
			registry.Register(&continuationAgent{provider: "codex"})
			server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), registry, ServerConfig{DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
			httpServer := httptest.NewServer(server.router)
			defer httpServer.Close()
			response := postV1Control(t, httpServer.URL, "boundary", "input", map[string]any{"idempotency_key": "idle-wake", "kind": "prompt", "text": "follow-up"})
			defer response.Body.Close()
			if !idle {
				if response.StatusCode != http.StatusConflict {
					t.Fatalf("unfinished lost prompt status=%d", response.StatusCode)
				}
				return
			}
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("idle lost prompt status=%d", response.StatusCode)
			}
			waitForDurableTerminal(t, store, "boundary")
			receipt, err := store.GetTerminalReceipt(ctx, "boundary")
			if err != nil || receipt.Generation != 3 || receipt.State != durable.StateCompleted {
				t.Fatalf("idle continuation receipt=%+v err=%v", receipt, err)
			}
		})
	}
}
