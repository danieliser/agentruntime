// Package eventstream commits native records before publishing them and
// provides race-free stored-then-live replay from durable sequence cursors.
package eventstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

const SchemaVersion = "1.0"

const (
	defaultLiveBuffer = 64
	maxLiveBuffer     = 4096
)

// IngestParams identifies the logical/generation owner of one native record.
type IngestParams struct {
	SessionID  string
	Generation int64
	Record     nativeprotocol.Record
}

// TerminalParams describes AgentD's process-level terminal control event. It
// is appended before the immutable terminal receipt is finalized.
type TerminalParams struct {
	SessionID  string
	Generation int64
	Timestamp  time.Time
	Reason     string
	ExitCode   int
	Signal     string
	Error      string
}

// Broker is the one canonical append and subscription entry point.
type Broker struct {
	store durable.Store
	mu    sync.Mutex
	state map[string]*sessionState

	// afterCommit is a fault-injection seam used to prove recovery from the
	// append/publish crash boundary.
	afterCommit func(durable.Event) error
}

type sessionState struct {
	mu             sync.Mutex
	nextSubscriber uint64
	subscribers    map[uint64]*liveSubscriber
}

type liveSubscriber struct {
	events chan durable.Event
	done   <-chan struct{}
	mu     sync.RWMutex
	err    error
}

func (subscriber *liveSubscriber) fail(err error) {
	subscriber.mu.Lock()
	subscriber.err = err
	subscriber.mu.Unlock()
}

func (subscriber *liveSubscriber) failure() error {
	subscriber.mu.RLock()
	defer subscriber.mu.RUnlock()
	return subscriber.err
}

// New constructs a broker over the AgentD durable store.
func New(store durable.Store) *Broker {
	return &Broker{store: store, state: make(map[string]*sessionState)}
}

// Ingest derives one typed envelope, commits it, and only then publishes the
// committed identity to live subscribers.
func (broker *Broker) Ingest(ctx context.Context, params IngestParams) (durable.Event, error) {
	const op = "ingest_native_record"
	if broker == nil || broker.store == nil {
		return durable.Event{}, durable.NewError(durable.CodeInvalidState, op, "durable store is required", nil)
	}
	if params.SessionID == "" || params.Generation < 1 || params.Record.Ordinal < 1 || params.Record.Timestamp.IsZero() {
		return durable.Event{}, durable.NewError(durable.CodeInvalidArgument, op, "session, generation, ordinal, and timestamp are required", nil)
	}
	eventType, payload, stream, providerID, err := derive(params.Record)
	if err != nil {
		return durable.Event{}, err
	}
	if providerID != "" {
		if _, err := broker.store.BindGenerationProvider(ctx, durable.BindGenerationProviderParams{
			SessionID: params.SessionID, Generation: params.Generation,
			ProviderID: providerID, At: params.Record.Timestamp,
		}); err != nil {
			return durable.Event{}, err
		}
	}
	state := broker.session(params.SessionID)
	state.mu.Lock()
	defer state.mu.Unlock()
	result, err := broker.store.AppendEvent(ctx, durable.AppendEventParams{
		SchemaVersion: SchemaVersion,
		EventID:       sourceEventID(params),
		SessionID:     params.SessionID,
		Generation:    params.Generation,
		Timestamp:     params.Record.Timestamp,
		Type:          eventType,
		Stream:        stream,
		Payload:       payload,
		Raw:           params.Record.Raw,
	})
	if err != nil {
		return durable.Event{}, err
	}
	if !result.Created {
		return result.Event, nil
	}
	if broker.afterCommit != nil {
		if err := broker.afterCommit(result.Event); err != nil {
			return durable.Event{}, err
		}
	}
	broker.publishLocked(state, result.Event)
	return result.Event, nil
}

// IngestTerminal commits AgentD's terminal control event through the same
// sequence allocator and live publication path as native records.
func (broker *Broker) IngestTerminal(ctx context.Context, params TerminalParams) (durable.Event, error) {
	const op = "ingest_terminal_event"
	if broker == nil || broker.store == nil {
		return durable.Event{}, durable.NewError(durable.CodeInvalidState, op, "durable store is required", nil)
	}
	if params.SessionID == "" || params.Generation < 1 || params.Timestamp.IsZero() || params.Reason == "" {
		return durable.Event{}, durable.NewError(durable.CodeInvalidArgument, op, "session, generation, timestamp, and reason are required", nil)
	}
	payload := map[string]any{"reason": params.Reason, "exit_code": params.ExitCode}
	if params.Signal != "" {
		payload["signal"] = params.Signal
	}
	if params.Error != "" {
		payload["error"] = params.Error
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return durable.Event{}, durable.NewError(durable.CodeIndeterminate, op, "encode terminal event", err)
	}
	state := broker.session(params.SessionID)
	state.mu.Lock()
	defer state.mu.Unlock()
	result, err := broker.store.AppendEvent(ctx, durable.AppendEventParams{
		SchemaVersion: SchemaVersion, EventID: terminalEventID(params.SessionID, params.Generation),
		SessionID: params.SessionID, Generation: params.Generation, Timestamp: params.Timestamp,
		Type: "session." + params.Reason, Stream: durable.StreamTerminal,
		Payload: raw, Raw: raw,
	})
	if err != nil {
		return durable.Event{}, err
	}
	if !result.Created {
		return result.Event, nil
	}
	if broker.afterCommit != nil {
		if err := broker.afterCommit(result.Event); err != nil {
			return durable.Event{}, err
		}
	}
	broker.publishLocked(state, result.Event)
	return result.Event, nil
}

func (broker *Broker) publishLocked(state *sessionState, event durable.Event) {
	for id, subscriber := range state.subscribers {
		select {
		case <-subscriber.done:
			delete(state.subscribers, id)
			close(subscriber.events)
		default:
			select {
			case subscriber.events <- event:
			default:
				subscriber.fail(durable.NewError(durable.CodeBackpressure, "publish_live_event", "live subscriber fell behind; reconnect from the last delivered sequence", nil))
				delete(state.subscribers, id)
				close(subscriber.events)
			}
		}
	}
}

func (broker *Broker) session(sessionID string) *sessionState {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	state := broker.state[sessionID]
	if state == nil {
		state = &sessionState{subscribers: make(map[uint64]*liveSubscriber)}
		broker.state[sessionID] = state
	}
	return state
}

func derive(record nativeprotocol.Record) (string, json.RawMessage, durable.Stream, string, error) {
	switch record.Stream {
	case nativeprotocol.StreamProviderStdout:
		adapter, err := nativeprotocol.NewAdapter(record.Provider)
		if err != nil {
			return "", nil, "", "", durable.NewError(durable.CodeInvalidArgument, "derive_native_record", "unsupported provider", err)
		}
		derived, err := adapter.Decode(record.Raw)
		if err != nil {
			return "", nil, "", "", durable.NewError(durable.CodeIndeterminate, "derive_native_record", "decode provider record", err)
		}
		return derived.Type, derived.Payload, durable.StreamProviderStdout, derived.ProviderID, nil
	case nativeprotocol.StreamRuntimeStderr:
		payload, err := json.Marshal(map[string]string{"text": string(record.Raw)})
		if err != nil {
			return "", nil, "", "", durable.NewError(durable.CodeIndeterminate, "derive_native_record", "encode stderr payload", err)
		}
		return "runtime.stderr", payload, durable.StreamRuntimeStderr, "", nil
	default:
		return "", nil, "", "", durable.NewError(durable.CodeInvalidArgument, "derive_native_record", "unsupported native stream", nil)
	}
}

// sourceEventID binds an immutable event ID to the source position, not its
// content. A changed record at the same position therefore becomes an explicit
// immutable conflict instead of silently creating a second event.
func sourceEventID(params IngestParams) string {
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%s\x00%d\x00%s\x00%d", params.SessionID, params.Generation, params.Record.Stream, params.Record.Ordinal)
	return "evt_" + hex.EncodeToString(digest.Sum(nil))
}

func terminalEventID(sessionID string, generation int64) string {
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%s\x00%d\x00terminal", sessionID, generation)
	return "evt_" + hex.EncodeToString(digest.Sum(nil))
}
