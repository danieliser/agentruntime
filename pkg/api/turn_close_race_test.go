package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// exitingTransport models a provider that is shutting down after its turn:
// every write fails.
type exitingTransport struct {
	nativeprotocol.Transport
	sends *atomic.Int32
}

func (t exitingTransport) Send(context.Context, nativeprotocol.Input) error {
	t.sends.Add(1)
	return errors.New("write |1: broken pipe")
}

func closingTurnServer(t *testing.T, closesAfterTurn bool) (*Server, durable.Store, string, *activeNativeSession, *atomic.Int32) {
	t.Helper()
	store, id := runningRecoveryStore(t)
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store)})
	server.turnCloseWait = 2 * time.Second
	sends := &atomic.Int32{}
	active := server.setNativeTransport(id, exitingTransport{sends: sends})
	active.setClosesAfterTurn(closesAfterTurn)
	active.turnCompleted()
	return server, store, id, active, sends
}

func finalizeExitedGeneration(t *testing.T, server *Server, store durable.Store, id string, active *activeNativeSession) {
	t.Helper()
	server.clearNativeTransport(id, active)
	now := time.Now().UTC()
	if _, err := store.FinalizeSession(context.Background(), durable.FinalizeSessionParams{From: durable.StateRunning, GenerationFrom: durable.GenerationRunning, GenerationTo: durable.GenerationExited, Receipt: durable.TerminalReceipt{SessionID: id, Generation: 1, State: durable.StateCompleted, StartedAt: now.Add(-time.Second), EndedAt: now, OutputHash: "out"}}); err != nil {
		t.Fatal(err)
	}
}

func controlEvents(t *testing.T, store durable.Store, id string) int {
	t.Helper()
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: id, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Events)
}

func TestPromptDuringTurnCloseWaitsForExitThenWakes(t *testing.T) {
	server, store, id, active, sends := closingTurnServer(t, true)
	result := make(chan error, 1)
	go func() {
		_, err := server.sendSessionInput(context.Background(), id, "close-race", nativeprotocol.InputPrompt, "next")
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("prompt returned before the provider exited: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if n := controlEvents(t, store, id); n != 0 {
		t.Fatalf("waiting prompt wrote %d events before exit", n)
	}
	finalizeExitedGeneration(t, server, store, id, active)
	var err error
	select {
	case err = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt never resumed after exit")
	}
	if sends.Load() != 0 {
		t.Fatalf("prompt was written to the exiting provider %d times", sends.Load())
	}
	t.Logf("retry result: %v", err)
	// The retry takes the wake path; this fixture has no runnable provider, so
	// only the old failure is forbidden.
	if err != nil && (durable.IsCode(err, durable.CodeIndeterminate) || strings.Contains(err.Error(), "still closing")) {
		t.Fatalf("prompt did not take the wake path: %v", err)
	}
}

func TestSteerDuringTurnCloseAsksForPrompt(t *testing.T) {
	server, store, id, _, sends := closingTurnServer(t, true)
	_, err := server.sendSessionInput(context.Background(), id, "close-steer", nativeprotocol.InputSteer, "more")
	if !durable.IsCode(err, durable.CodeInvalidState) || !strings.Contains(err.Error(), "use prompt") {
		t.Fatalf("steer err=%v", err)
	}
	if sends.Load() != 0 || controlEvents(t, store, id) != 0 {
		t.Fatalf("steer touched the exiting provider: sends=%d", sends.Load())
	}
}

func TestPromptDuringTurnCloseTimesOutCleanly(t *testing.T) {
	server, store, id, _, sends := closingTurnServer(t, true)
	server.turnCloseWait = 100 * time.Millisecond
	_, err := server.sendSessionInput(context.Background(), id, "close-timeout", nativeprotocol.InputPrompt, "next")
	if !durable.IsCode(err, durable.CodeInvalidState) || !strings.Contains(err.Error(), "still closing") {
		t.Fatalf("err=%v", err)
	}
	if sends.Load() != 0 || controlEvents(t, store, id) != 0 {
		t.Fatalf("timed-out prompt left side effects: sends=%d", sends.Load())
	}
}

func TestInteractiveIdleSessionStillTakesPromptOnLiveTransport(t *testing.T) {
	server, _, id, _, sends := closingTurnServer(t, false)
	_, err := server.sendSessionInput(context.Background(), id, "live-prompt", nativeprotocol.InputPrompt, "next")
	if sends.Load() != 1 || !durable.IsCode(err, durable.CodeIndeterminate) {
		t.Fatalf("interactive prompt sends=%d err=%v", sends.Load(), err)
	}
}

func TestInterruptDuringTurnCloseWritesNoIntent(t *testing.T) {
	server, store, id, _, _ := closingTurnServer(t, true)
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	response := postV1Control(t, httpServer.URL, id, "interrupt", map[string]any{"idempotency_key": "close-interrupt"})
	defer response.Body.Close()
	var envelope struct {
		Error apiErrorEnvelope `json:"error"`
	}
	decodeJSON(t, response.Body, &envelope)
	if response.StatusCode != http.StatusConflict || envelope.Error.Code != durable.CodeInvalidState {
		t.Fatalf("status=%d error=%+v", response.StatusCode, envelope.Error)
	}
	if n := controlEvents(t, store, id); n != 0 {
		t.Fatalf("interrupt wrote %d events on a closing turn", n)
	}
}
