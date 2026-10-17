package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/session"
)

// Failure before durable intent must still close an already-reopened session.
type rejectWakeIntentStore struct{ durable.Store }

func (s rejectWakeIntentStore) AppendEvent(ctx context.Context, p durable.AppendEventParams) (durable.AppendEventResult, error) {
	if p.Type == "control.prompt.requested" {
		return durable.AppendEventResult{}, durable.NewError(durable.CodeIndeterminate, "fixture", "intent write failed", nil)
	}
	return s.Store.AppendEvent(ctx, p)
}
func TestWakeIntentWriteFailureGetsTerminalReceipt(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedContinuation(t, store, durable.StateCompleted, "provider")
	failing := rejectWakeIntentStore{store}
	rt := &rejectedWakeRuntime{}
	server := NewServer(session.NewManager(), rt, agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: failing, EventBroker: eventstream.New(failing)})
	if _, err := server.sendSessionInput(context.Background(), "boundary", "no-intent", nativeprotocol.InputPrompt, "follow-up"); err == nil {
		t.Fatal("intent failure ignored")
	}
	receipt, err := store.GetTerminalReceipt(context.Background(), "boundary")
	if err != nil || receipt.State != durable.StateFailed || receipt.Generation != 2 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if rt.spawns.Load() != 0 {
		t.Fatal("spawned without intent")
	}
	if err := store.CheckIntegrity(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryFinishesUnstartedWakeSettlement(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedContinuation(t, store, durable.StateCompleted, "provider")
	ctx := context.Background()
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "boundary", From: durable.StateCompleted, To: durable.StateStarting}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "boundary", Runtime: "local", ContainerID: unstartedRuntimeIdentityPrefix + "boundary:2"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(session.NewManager(), &rejectedWakeRuntime{}, agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store)})
	server.RestoreRecoveredSessions(nil, "local")
	receipt, err := store.GetTerminalReceipt(ctx, "boundary")
	if err != nil || receipt.State != durable.StateIndeterminate || receipt.Generation != 2 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
}
