package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type blockedWakeRuntime struct{ entered, release chan struct{} }

func (*blockedWakeRuntime) Name() string { return "local" }
func (r *blockedWakeRuntime) Spawn(context.Context, runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	close(r.entered)
	<-r.release
	return nil, &runtime.SpawnError{Reason: "fixture"}
}
func (*blockedWakeRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) { return nil, nil }
func (*blockedWakeRuntime) Cleanup(context.Context) error                            { return nil }

func TestBlockedWakeDoesNotBlockOtherSessionInputOrFinalization(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedContinuation(t, store, durable.StateCompleted, "provider")
	ctx := context.Background()
	_, err = store.CreateSession(ctx, durable.CreateSessionParams{SessionID: "other", IdempotencyKey: "other", RequestHash: "other", RequestManifest: []byte(`{}`), Agent: "codex", Runtime: "local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range []durable.TransitionSessionParams{{SessionID: "other", From: durable.StateCreated, To: durable.StateStarting}, {SessionID: "other", From: durable.StateStarting, To: durable.StateRunning}} {
		if _, err := store.TransitionSession(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "other", Runtime: "local", ContainerID: "other"}); err != nil {
		t.Fatal(err)
	}
	rt := &blockedWakeRuntime{make(chan struct{}), make(chan struct{})}
	server := NewServer(session.NewManager(), rt, agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store)})
	server.setNativeTransport("other", continuationInputProbe{}).turnCompleted()
	wakeDone := make(chan struct{})
	go func() {
		defer close(wakeDone)
		server.sendSessionInput(ctx, "boundary", "blocked", nativeprotocol.InputPrompt, "follow-up")
	}()
	<-rt.entered
	defer func() { close(rt.release); <-wakeDone }()
	done := make(chan error, 1)
	go func() {
		_, err := server.sendSessionInput(ctx, "other", "input", nativeprotocol.InputPrompt, "next")
		if err == nil {
			server.finalizeV1Session("other", runtime.ExitResult{Code: 0})
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Error("other session blocked behind wake")
		return
	}
	receipt, err := store.GetTerminalReceipt(ctx, "other")
	if err != nil || receipt.State != durable.StateCompleted {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}
