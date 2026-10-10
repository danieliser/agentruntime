package eventstream

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func TestClaudeSupersededBoundaryRetainsNativeRawAndReplayIdentity(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "steer-boundary")
	broker := New(store)
	raw := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming","usage":{"input_tokens":4,"output_tokens":21,"cache_creation_input_tokens":100,"cache_read_input_tokens":200},"total_cost_usd":0.0123}`)
	params := IngestParams{SessionID: "steer-boundary", Generation: 1, Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: 1, Timestamp: time.Unix(200, 0).UTC(), Raw: raw, SupersededBySteer: true}}
	event, err := broker.Ingest(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != "turn.superseded" || string(event.Raw) != string(raw) {
		t.Fatalf("unexpected mapped event: %+v", event)
	}
	var payload struct {
		Superseded   bool   `json:"superseded_by_steer"`
		OriginalType string `json:"superseded_event_type"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Superseded || payload.OriginalType != "turn.failed" {
		t.Fatalf("superseded payload=%+v", payload)
	}
	replay, err := broker.Ingest(ctx, params)
	if err != nil || replay.EventID != event.EventID || replay.Sequence != event.Sequence {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	params.Record.Ordinal = 2
	params.Record.SupersededBySteer = false
	finalRaw := []byte(`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":7,"output_tokens":34,"cache_creation_input_tokens":120,"cache_read_input_tokens":250},"total_cost_usd":0.0234}`)
	params.Record.Raw = finalRaw
	final, err := broker.Ingest(ctx, params)
	if err != nil || final.Type != "turn.completed" || string(final.Raw) != string(finalRaw) {
		t.Fatalf("replacement numeric receipt changed: %+v err=%v", final, err)
	}
	// These are independent native receipts; no cumulative usage assumption is made.
	params.Record.Ordinal = 3
	params.Record.SupersededBySteer = true
	params.Record.Raw = []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"api_error"}`)
	actual, err := broker.Ingest(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Type != "turn.failed" {
		t.Fatalf("real error masked: %+v", actual)
	}
}
