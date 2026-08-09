package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func TestV1EventReplayUsesSequenceCursor(t *testing.T) {
	ts, store, broker := newEventStreamTestServer(t, "replay-session")
	ingestAPIEvent(t, broker, "replay-session", 1)
	second := ingestAPIEvent(t, broker, "replay-session", 2)

	resp := get(t, ts, "/api/v1/sessions/replay-session/events?after_sequence=1&limit=10")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		APIVersion string `json:"api_version"`
		Data       struct {
			Events           []eventEnvelope `json:"events"`
			EarliestSequence int64           `json:"earliest_sequence"`
			LastSequence     int64           `json:"last_sequence"`
			HasMore          bool            `json:"has_more"`
		} `json:"data"`
	}
	decodeJSON(t, resp.Body, &body)
	if body.APIVersion != "v1" || len(body.Data.Events) != 1 || body.Data.Events[0].Sequence != 2 || body.Data.EarliestSequence != 1 || body.Data.LastSequence != 2 || body.Data.HasMore {
		t.Fatalf("replay response = %+v", body)
	}
	raw, err := base64.StdEncoding.DecodeString(body.Data.Events[0].RawBase64)
	if err != nil {
		t.Fatalf("decode raw event: %v", err)
	}
	if string(raw) != string(second.Raw) {
		t.Fatalf("raw replay = %q, want %q", raw, second.Raw)
	}
	stored, err := store.GetSession(context.Background(), "replay-session")
	if err != nil || stored.LastSequence != 2 {
		t.Fatalf("stored session = %+v err=%v", stored, err)
	}
}

func TestV1EventReplayRejectsFutureCursor(t *testing.T) {
	ts, _, _ := newEventStreamTestServer(t, "future-session")
	resp := get(t, ts, "/api/v1/sessions/future-session/events?after_sequence=1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, resp.Body, &body)
	if body.Error.Code != string(durable.CodeInvalidCursor) {
		t.Fatalf("error code = %q, want %q", body.Error.Code, durable.CodeInvalidCursor)
	}
}

func TestV1EventReplayPaginatesLargeHistory(t *testing.T) {
	ts, _, broker := newEventStreamTestServer(t, "large-replay-session")
	for ordinal := int64(1); ordinal <= 1001; ordinal++ {
		ingestAPIEvent(t, broker, "large-replay-session", ordinal)
	}

	resp := get(t, ts, "/api/v1/sessions/large-replay-session/events?after_sequence=0&limit=5000")
	defer resp.Body.Close()
	var first struct {
		Data eventPageEnvelope `json:"data"`
	}
	decodeJSON(t, resp.Body, &first)
	if resp.StatusCode != http.StatusOK || len(first.Data.Events) != 1000 || first.Data.Events[0].Sequence != 1 || first.Data.Events[999].Sequence != 1000 || !first.Data.HasMore {
		t.Fatalf("first large page: status=%d data=%+v", resp.StatusCode, first.Data)
	}

	resp = get(t, ts, "/api/v1/sessions/large-replay-session/events?after_sequence=1000&limit=1000")
	defer resp.Body.Close()
	var final struct {
		Data eventPageEnvelope `json:"data"`
	}
	decodeJSON(t, resp.Body, &final)
	if resp.StatusCode != http.StatusOK || len(final.Data.Events) != 1 || final.Data.Events[0].Sequence != 1001 || final.Data.HasMore {
		t.Fatalf("final large page: status=%d data=%+v", resp.StatusCode, final.Data)
	}
}

func TestV1EventReplayRemainsAvailableAfterTerminalReceipt(t *testing.T) {
	ts, store, broker := newEventStreamTestServer(t, "terminal-replay-session")
	for ordinal := int64(1); ordinal <= 3; ordinal++ {
		ingestAPIEvent(t, broker, "terminal-replay-session", ordinal)
	}
	ctx := context.Background()
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "terminal-replay-session", From: durable.StateCreated, To: durable.StateStarting, At: time.Unix(300, 0).UTC()}); err != nil {
		t.Fatalf("transition session to starting: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "terminal-replay-session", From: durable.StateStarting, To: durable.StateRunning, At: time.Unix(301, 0).UTC()}); err != nil {
		t.Fatalf("transition session to running: %v", err)
	}
	if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{SessionID: "terminal-replay-session", Generation: 1, From: durable.GenerationStarting, To: durable.GenerationRunning, At: time.Unix(301, 0).UTC()}); err != nil {
		t.Fatalf("transition generation to running: %v", err)
	}
	exitCode := 0
	if _, err := store.FinalizeSession(ctx, durable.FinalizeSessionParams{
		From: durable.StateRunning, GenerationFrom: durable.GenerationRunning, GenerationTo: durable.GenerationExited,
		Receipt: durable.TerminalReceipt{SessionID: "terminal-replay-session", Generation: 1, State: durable.StateCompleted, ExitCode: &exitCode, StartedAt: time.Unix(301, 0).UTC(), EndedAt: time.Unix(302, 0).UTC(), OutputHash: "sha256:terminal-output", LastSequence: 3},
	}); err != nil {
		t.Fatalf("finalize session: %v", err)
	}

	resp := get(t, ts, "/api/v1/sessions/terminal-replay-session/events?after_sequence=1&limit=10")
	defer resp.Body.Close()
	var body struct {
		Data eventPageEnvelope `json:"data"`
	}
	decodeJSON(t, resp.Body, &body)
	if resp.StatusCode != http.StatusOK || len(body.Data.Events) != 2 || body.Data.Events[0].Sequence != 2 || body.Data.LastSequence != 3 {
		t.Fatalf("terminal replay: status=%d data=%+v", resp.StatusCode, body.Data)
	}
}

func TestV1EventWebSocketHandsOffReplayToLiveExactlyOnce(t *testing.T) {
	ts, _, broker := newEventStreamTestServer(t, "ws-session")
	ingestAPIEvent(t, broker, "ws-session", 1)
	ingestAPIEvent(t, broker, "ws-session", 2)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws/sessions/ws-session/events?after_sequence=1"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial event stream: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var ready streamReadyFrame
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("read stream readiness: %v", err)
	}
	if ready.FrameType != "stream.ready" || ready.AfterSequence != 1 || ready.EarliestSequence != 1 || ready.ReplayThrough != 2 {
		t.Fatalf("stream readiness = %+v", ready)
	}
	var replay eventEnvelope
	if err := conn.ReadJSON(&replay); err != nil {
		t.Fatalf("read replay event: %v", err)
	}
	if replay.Sequence != 2 {
		t.Fatalf("replay sequence = %d, want 2", replay.Sequence)
	}
	ingestAPIEvent(t, broker, "ws-session", 3)
	var live eventEnvelope
	if err := conn.ReadJSON(&live); err != nil {
		t.Fatalf("read live event: %v", err)
	}
	if live.Sequence != 3 || live.EventID == replay.EventID {
		t.Fatalf("live event = %+v after replay %+v", live, replay)
	}
}

func newEventStreamTestServer(t *testing.T, sessionID string) (*httptest.Server, durable.Store, *eventstream.Broker) {
	t.Helper()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	_, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: sessionID, IdempotencyKey: "job-" + sessionID, RequestHash: "sha256:" + sessionID,
		RequestManifest: json.RawMessage(`{"agent":"claude","runtime":"docker"}`),
		Agent:           "claude", Runtime: "docker", CreatedAt: time.Unix(100, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("create durable session: %v", err)
	}
	_, err = store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: sessionID, Runtime: "docker", ContainerID: "container-" + sessionID,
		ImageReference: "agent:fixture", ImageDigest: "sha256:image", SandboxProfile: "sandbox-v1",
		CreatedAt: time.Unix(101, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("create durable generation: %v", err)
	}
	broker := eventstream.New(store)
	reg := agent.NewRegistry()
	ts, _ := newConfiguredTestServer(t, reg, ServerConfig{DurableStore: store, EventBroker: broker})
	return ts, store, broker
}

func ingestAPIEvent(t *testing.T, broker *eventstream.Broker, sessionID string, ordinal int64) durable.Event {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"%d"}}}`, ordinal))
	event, err := broker.Ingest(context.Background(), eventstream.IngestParams{
		SessionID: sessionID, Generation: 1,
		Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: ordinal, Timestamp: time.Unix(200+ordinal, 0).UTC(), Raw: raw},
	})
	if err != nil {
		t.Fatalf("ingest API event %d: %v", ordinal, err)
	}
	return event
}
