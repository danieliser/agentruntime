package api

import (
	"context"
	"fmt"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
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

func TestRecoveredNonMaintainedSessionRestoresIdle(t *testing.T) {
	store, id := runningRecoveryStore(t)
	ctx := context.Background()
	for i, kind := range []string{"turn.completed", "usage.updated"} {
		if _, err := store.AppendEvent(ctx, durable.AppendEventParams{SessionID: id, Generation: 1, EventID: fmt.Sprintf("idle-%d", i), SchemaVersion: "1.0", Timestamp: time.Now(), Type: kind, Stream: durable.StreamProviderStdout, Raw: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	handle := newBlockingRecoveredNativeTestHandle(id, "missing-container")
	manager := session.NewManager()
	recovered := manager.Recover([]runtime.ProcessHandle{handle}, "docker")
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
	server.RestoreRecoveredSessions(recovered, "docker")
	defer func() {
		if err := handle.Kill(); err != nil {
			t.Error(err)
		}
		waitForDurableTerminal(t, store, id)
	}()
	active := server.nativeSession(id)
	if active == nil {
		t.Fatal("missing recovered transport")
	}
	active.mu.Lock()
	idle := active.idle
	active.mu.Unlock()
	if !idle {
		t.Fatal("non-maintained recovered session rejected its completed turn")
	}
	if _, err := server.sendSessionInput(ctx, id, "after-restart", nativeprotocol.InputPrompt, "next"); err != nil {
		t.Fatal(err)
	}
}

type eventReadCounter struct {
	durable.Store
	reads int
}

func (s *eventReadCounter) ListEvents(ctx context.Context, q durable.EventQuery) (durable.EventPage, error) {
	s.reads++
	return s.Store.ListEvents(ctx, q)
}
func TestLostIdleReadoutCachesDurableBoundary(t *testing.T) {
	store, id := runningRecoveryStore(t)
	ctx := context.Background()
	for i, kind := range []string{"turn.completed", "usage.updated"} {
		if _, err := store.AppendEvent(ctx, durable.AppendEventParams{SessionID: id, Generation: 1, EventID: fmt.Sprintf("boundary-%d", i), SchemaVersion: "1.0", Timestamp: time.Now(), Type: kind, Stream: durable.StreamProviderStdout, Raw: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	counted := &eventReadCounter{Store: store}
	server := NewServer(session.NewManager(), &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: counted, EventBroker: eventstream.New(counted)})
	stored, _ := store.GetSession(ctx, id)
	for range 10 {
		if !server.generationEndedNormally(ctx, stored, 1) {
			t.Fatal("lost idle state not recognized")
		}
	}
	if counted.reads != 1 {
		t.Fatalf("GETs rescanned durable history %d times", counted.reads)
	}
	// A later intent invalidates the cached boundary.
	if _, err := store.AppendEvent(ctx, durable.AppendEventParams{SessionID: id, Generation: 1, EventID: "next-intent", SchemaVersion: "1.0", Timestamp: time.Now(), Type: "control.prompt.requested", Stream: durable.StreamControl, Raw: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	stored, _ = store.GetSession(ctx, id)
	if server.generationEndedNormally(ctx, stored, 1) {
		t.Fatal("stale idle cache accepted later input")
	}
}
