package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func TestContinuationDeniesStoppedOrUnrecoverableSessions(t *testing.T) {
	for _, state := range []durable.SessionState{durable.StateCancelled, durable.StateFailed, durable.StateTimedOut, durable.StateCrashed, durable.StateIndeterminate, durable.StateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			seedContinuation(t, store, state, "") // Completed without provider identity is unrecoverable.
			server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store)})
			httpServer := httptest.NewServer(server.router)
			defer httpServer.Close()
			response := postV1Control(t, httpServer.URL, "boundary", "input", map[string]any{"idempotency_key": "deny", "kind": "prompt", "text": "continue"})
			defer response.Body.Close()
			var envelope struct {
				Error apiErrorEnvelope `json:"error"`
			}
			decodeJSON(t, response.Body, &envelope)
			if response.StatusCode != http.StatusConflict || envelope.Error.Code != durable.CodeInvalidState || envelope.Error.Message == "" {
				t.Fatalf("status=%d error=%+v", response.StatusCode, envelope.Error)
			}
			stored, _ := store.GetSession(context.Background(), "boundary")
			if stored.ActiveGeneration != 1 || stored.State != state {
				t.Fatalf("denial mutated session: %+v", stored)
			}
		})
	}
}

func TestPromptDuringActiveTurnPointsToSteerBeforeWritingIntent(t *testing.T) {
	store, id := runningRecoveryStore(t)
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store)})
	server.setNativeTransport(id, continuationInputProbe{})
	_, err := server.sendSessionInput(context.Background(), id, "active-prompt", nativeprotocol.InputPrompt, "extra")
	if !durable.IsCode(err, durable.CodeInvalidState) || !strings.Contains(err.Error(), "steer") {
		t.Fatalf("active prompt err=%v", err)
	}
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: id, Limit: 100})
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("rejected prompt committed intent: %+v err=%v", page, err)
	}
}

func TestStructuredResultSelectsGenerationAndNeverLeaksPreviousArtifact(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedContinuation(t, store, durable.StateCompleted, "provider")
	ctx := context.Background()
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store)})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "boundary", From: durable.StateCompleted, To: durable.StateStarting}); err != nil {
		t.Fatal(err)
	}
	gen, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "boundary", Runtime: "local", ContainerID: "generation-2"})
	if err != nil {
		t.Fatal(err)
	}
	// The old result is within the tail window while generation two is starting.
	response := get(t, httpServer, "/api/v1/sessions/boundary/result")
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("new generation leaked old result: %d", response.StatusCode)
	}
	response = get(t, httpServer, "/api/v1/sessions/boundary/result?generation=1")
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(data) != `{"first":true}` {
		t.Fatalf("historical result status=%d body=%s", response.StatusCode, data)
	}
	response = get(t, httpServer, "/api/v1/sessions/boundary/receipt?generation=1")
	defer response.Body.Close()
	var receipt struct {
		Data v1TerminalReceiptData `json:"data"`
	}
	decodeJSON(t, response.Body, &receipt)
	if response.StatusCode != http.StatusOK || receipt.Data.Generation != gen.Number-1 {
		t.Fatalf("historical receipt=%+v", receipt)
	}
}

func seedContinuation(t *testing.T, store durable.Store, state durable.SessionState, providerID string) {
	t.Helper()
	ctx := context.Background()
	_, err := store.CreateSession(ctx, durable.CreateSessionParams{SessionID: "boundary", IdempotencyKey: "boundary-key", RequestHash: "hash", RequestManifest: json.RawMessage(`{"agent":"codex","runtime":"local"}`), Agent: "codex", Runtime: "local"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "boundary", From: durable.StateCreated, To: durable.StateStarting})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "boundary", Runtime: "local", ContainerID: "first", ProviderID: providerID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.TransitionGeneration(ctx, durable.TransitionGenerationParams{SessionID: "boundary", Generation: 1, From: durable.GenerationStarting, To: durable.GenerationRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "boundary", From: durable.StateStarting, To: durable.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendEvent(ctx, durable.AppendEventParams{SessionID: "boundary", Generation: 1, EventID: "first-result", SchemaVersion: "v1", Timestamp: time.Now(), Type: "output.final", Stream: durable.StreamTerminal, Raw: []byte(`{"first":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.FinalizeSession(ctx, durable.FinalizeSessionParams{From: durable.StateRunning, GenerationFrom: durable.GenerationRunning, GenerationTo: durable.GenerationExited, Receipt: durable.TerminalReceipt{SessionID: "boundary", Generation: 1, State: state, StartedAt: gen.CreatedAt, EndedAt: time.Now(), OutputHash: "first-output", LastSequence: 1}})
	if err != nil {
		t.Fatal(err)
	}
}

type continuationInputProbe struct{ nativeprotocol.Transport }

func (continuationInputProbe) Send(context.Context, nativeprotocol.Input) error { return nil }
