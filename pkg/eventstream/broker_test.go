package eventstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func TestIngestCommitsBeforePublishAndReplaysAfterFault(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-fault")
	broker := New(store)
	fault := errors.New("simulated crash after commit")
	broker.afterCommit = func(durable.Event) error { return fault }

	raw := []byte(`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"committed"}}}`)
	_, err := broker.Ingest(ctx, IngestParams{
		SessionID: "session-fault", Generation: 1,
		Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: 1, Timestamp: time.Unix(200, 0).UTC(), Raw: raw},
	})
	if !errors.Is(err, fault) {
		t.Fatalf("ingest error = %v, want injected fault", err)
	}
	broker.afterCommit = nil

	subscription, err := broker.Subscribe(ctx, "session-fault", 0, 4)
	if err != nil {
		t.Fatalf("subscribe after fault: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	event, err := subscription.Next(ctx)
	if err != nil {
		t.Fatalf("replay committed event: %v", err)
	}
	if event.Sequence != 1 || event.Type != "content.delta" || string(event.Raw) != string(raw) {
		t.Fatalf("replayed event = %+v", event)
	}
}

func TestSubscribeClosesStoredThenLiveRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-race")
	broker := New(store)
	for ordinal := int64(1); ordinal <= 3; ordinal++ {
		ingestClaudeDelta(t, ctx, broker, "session-race", ordinal)
	}

	subscription, err := broker.Subscribe(ctx, "session-race", 1, 4)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	ingestErr := make(chan error, 1)
	go func() {
		raw := []byte(`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"4"}}}`)
		_, err := broker.Ingest(ctx, IngestParams{
			SessionID: "session-race", Generation: 1,
			Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: 4, Timestamp: time.Unix(204, 0).UTC(), Raw: raw},
		})
		ingestErr <- err
	}()
	for want := int64(2); want <= 4; want++ {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatalf("next sequence %d: %v", want, err)
		}
		if event.Sequence != want {
			t.Fatalf("sequence = %d, want %d", event.Sequence, want)
		}
	}
	if err := <-ingestErr; err != nil {
		t.Fatalf("live ingest: %v", err)
	}
}

func TestIngestKeepsStderrSeparateAndEventIDsStable(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-streams")
	broker := New(store)
	params := IngestParams{
		SessionID: "session-streams", Generation: 1,
		Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderCodex, Stream: nativeprotocol.StreamRuntimeStderr, Ordinal: 1, Timestamp: time.Unix(300, 0).UTC(), Raw: []byte("warning")},
	}
	first, err := broker.Ingest(ctx, params)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := broker.Ingest(ctx, params)
	if err != nil {
		t.Fatalf("repeat ingest: %v", err)
	}
	if first.EventID != second.EventID || first.Sequence != second.Sequence {
		t.Fatalf("stable event identity = %+v then %+v", first, second)
	}
	changed := params
	changed.Record.Raw = []byte("different warning")
	if _, err := broker.Ingest(ctx, changed); !durable.IsCode(err, durable.CodeImmutableConflict) {
		t.Fatalf("changed source position error = %v, want %s", err, durable.CodeImmutableConflict)
	}
	if first.Stream != durable.StreamRuntimeStderr || first.Type != "runtime.stderr" {
		t.Fatalf("stderr envelope = %+v", first)
	}
	page, err := store.ListEvents(ctx, durable.EventQuery{SessionID: "session-streams"})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("stored events = %d, want 1", len(page.Events))
	}
}

func TestIngestBindsProviderIdentityFromNativeRecord(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-provider-identity")
	broker := New(store)
	raw := []byte(`{"type":"system","subtype":"init","session_id":"claude-native-session"}`)
	_, err := broker.Ingest(ctx, IngestParams{
		SessionID: "session-provider-identity", Generation: 1,
		Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: 1, Timestamp: time.Unix(310, 0).UTC(), Raw: raw},
	})
	if err != nil {
		t.Fatalf("ingest provider identity: %v", err)
	}
	generation, err := store.GetGeneration(ctx, "session-provider-identity", 1)
	if err != nil {
		t.Fatalf("get generation: %v", err)
	}
	if generation.ProviderID != "claude-native-session" {
		t.Fatalf("provider ID = %q, want claude-native-session", generation.ProviderID)
	}
}

func TestIngestTerminalCommitsOneStableTerminalEvent(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-terminal-event")
	broker := New(store)
	params := TerminalParams{
		SessionID: "session-terminal-event", Generation: 1,
		Timestamp: time.Unix(320, 0).UTC(), Reason: "completed", ExitCode: 0,
	}
	first, err := broker.IngestTerminal(ctx, params)
	if err != nil {
		t.Fatalf("first terminal ingest: %v", err)
	}
	second, err := broker.IngestTerminal(ctx, params)
	if err != nil {
		t.Fatalf("repeat terminal ingest: %v", err)
	}
	if first.EventID != second.EventID || first.Sequence != second.Sequence || first.Stream != durable.StreamTerminal || first.Type != "session.completed" {
		t.Fatalf("terminal events = %+v then %+v", first, second)
	}
	page, err := store.ListEvents(ctx, durable.EventQuery{SessionID: params.SessionID})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("stored terminal events = %+v err=%v", page, err)
	}
}

func TestControlLedgerIsIdempotentAndExposesAmbiguousDispatch(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-controls")
	broker := New(store)
	params := ControlParams{
		SessionID: "session-controls", Generation: 1, IdempotencyKey: "control-key",
		Timestamp: time.Unix(330, 0).UTC(), Kind: "prompt", Payload: json.RawMessage(`{"text":"hello"}`),
	}
	begin, err := broker.BeginControl(ctx, params)
	if err != nil || begin.AlreadyDispatched {
		t.Fatalf("begin control = %+v err=%v", begin, err)
	}
	if _, err := broker.CompleteControl(ctx, params); err != nil {
		t.Fatalf("complete control: %v", err)
	}
	repeat, err := broker.BeginControl(ctx, params)
	if err != nil || !repeat.AlreadyDispatched {
		t.Fatalf("repeat completed control = %+v err=%v", repeat, err)
	}

	ambiguous := params
	ambiguous.IdempotencyKey = "ambiguous-key"
	if _, err := broker.BeginControl(ctx, ambiguous); err != nil {
		t.Fatalf("begin ambiguous control: %v", err)
	}
	if _, err := broker.BeginControl(ctx, ambiguous); !durable.IsCode(err, durable.CodeIndeterminate) {
		t.Fatalf("ambiguous retry error = %v, want %s", err, durable.CodeIndeterminate)
	}

	changed := params
	changed.Payload = json.RawMessage(`{"text":"different"}`)
	if _, err := broker.BeginControl(ctx, changed); !durable.IsCode(err, durable.CodeImmutableConflict) {
		t.Fatalf("changed control reuse error = %v, want %s", err, durable.CodeImmutableConflict)
	}
}

func TestSubscribeRejectsFutureCursor(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-cursor")
	broker := New(store)
	_, err := broker.Subscribe(ctx, "session-cursor", 1, 1)
	if !durable.IsCode(err, durable.CodeInvalidCursor) {
		t.Fatalf("future cursor error = %v, want %s", err, durable.CodeInvalidCursor)
	}
}

func TestSlowSubscriberNeverBlocksDurableIngestion(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	createStreamSession(t, ctx, store, "session-backpressure")
	broker := New(store)
	subscription, err := broker.Subscribe(ctx, "session-backpressure", 0, 1)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	ingestClaudeDelta(t, ctx, broker, "session-backpressure", 1)

	done := make(chan error, 1)
	go func() {
		raw := []byte(`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"2"}}}`)
		_, err := broker.Ingest(ctx, IngestParams{
			SessionID: "session-backpressure", Generation: 1,
			Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: 2, Timestamp: time.Unix(202, 0).UTC(), Raw: raw},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ingest with slow subscriber: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("slow subscriber blocked durable ingestion")
	}

	first, err := subscription.Next(ctx)
	if err != nil || first.Sequence != 1 {
		t.Fatalf("buffered first event = %+v, err=%v", first, err)
	}
	if _, err := subscription.Next(ctx); !durable.IsCode(err, durable.CodeBackpressure) {
		t.Fatalf("overflowed subscription error = %v, want %s", err, durable.CodeBackpressure)
	}
	reconnected, err := broker.Subscribe(ctx, "session-backpressure", 1, 1)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer reconnected.Close()
	second, err := reconnected.Next(ctx)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("replayed second event = %+v, err=%v", second, err)
	}
}

func createStreamSession(t *testing.T, ctx context.Context, store durable.Store, sessionID string) {
	t.Helper()
	_, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: sessionID, IdempotencyKey: "job-" + sessionID, RequestHash: "sha256:" + sessionID,
		RequestManifest: json.RawMessage(`{"agent":"claude","runtime":"docker"}`),
		Agent:           "claude", Runtime: "docker", CreatedAt: time.Unix(100, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, err = store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: sessionID, Runtime: "docker", ContainerID: "container-" + sessionID,
		ImageReference: "agent:fixture", ImageDigest: "sha256:image", SandboxProfile: "sandbox-v1",
		CreatedAt: time.Unix(101, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("create generation: %v", err)
	}
}

func ingestClaudeDelta(t *testing.T, ctx context.Context, broker *Broker, sessionID string, ordinal int64) durable.Event {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"%d"}}}`, ordinal))
	event, err := broker.Ingest(ctx, IngestParams{
		SessionID: sessionID, Generation: 1,
		Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: ordinal, Timestamp: time.Unix(200+ordinal, 0).UTC(), Raw: raw},
	})
	if err != nil {
		t.Fatalf("ingest ordinal %d: %v", ordinal, err)
	}
	return event
}
