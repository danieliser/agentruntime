package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestWakeIntentWithoutDispatchNeverStartsAgain(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedContinuation(t, store, durable.StateCompleted, "fixture-thread")
	rt := &rejectedWakeRuntime{}
	registry := agent.NewRegistry()
	registry.Register(&continuationAgent{provider: "codex"})
	dataDir, logDir := t.TempDir(), filepath.Join(t.TempDir(), "logs")
	for attempt := 0; attempt < 3; attempt++ {
		// Reconstruct server state for each retry, as after a daemon restart.
		server := NewServer(session.NewManager(), rt, registry, ServerConfig{DataDir: dataDir, LogDir: logDir, DurableStore: store, EventBroker: eventstream.New(store)})
		httpServer := httptest.NewServer(server.router)
		response := postV1Control(t, httpServer.URL, "boundary", "input", map[string]any{"idempotency_key": "failed-wake", "kind": "prompt", "text": "follow-up"})
		var envelope struct {
			Error apiErrorEnvelope `json:"error"`
		}
		decodeJSON(t, response.Body, &envelope)
		response.Body.Close()
		httpServer.Close()
		if response.StatusCode < http.StatusBadRequest || envelope.Error.Message == "" {
			t.Fatalf("unproven wake status=%d error=%+v", response.StatusCode, envelope.Error)
		}
		if attempt > 0 && envelope.Error.Code != durable.CodeIndeterminate {
			t.Fatalf("retry error=%+v", envelope.Error)
		}
		stored, err := store.GetSession(context.Background(), "boundary")
		if err != nil {
			t.Fatal(err)
		}
		if status := server.sessionContinuation(context.Background(), stored); status.Continuable {
			t.Fatalf("unresolved wake became continuable: %+v", status)
		}
	}
	if rt.spawns.Load() != 1 {
		t.Fatalf("unproven wake repeated a spawn: %d", rt.spawns.Load())
	}
	first, err := store.GetGenerationReceipt(context.Background(), "boundary", 1)
	if err != nil || first.OutputHash != "first-output" {
		t.Fatalf("failed wake changed prior proof: %+v err=%v", first, err)
	}
}

type rejectedWakeRuntime struct{ spawns atomic.Int32 }

func (*rejectedWakeRuntime) Name() string { return "local" }
func (rt *rejectedWakeRuntime) Spawn(context.Context, runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	rt.spawns.Add(1)
	return nil, &runtime.SpawnError{Reason: "fixture wake spawn failure"}
}
func (*rejectedWakeRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) {
	return nil, nil
}
func (*rejectedWakeRuntime) Cleanup(context.Context) error { return nil }
