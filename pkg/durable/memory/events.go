package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// AppendEvent implements DUR-104 atomic sequence allocation and immutable append.
func (store *Store) AppendEvent(ctx context.Context, params durable.AppendEventParams) (durable.AppendEventResult, error) {
	const op = "append_event"
	if err := checkContext(ctx); err != nil {
		return durable.AppendEventResult{}, err
	}
	if err := validateAppendEvent(params); err != nil {
		return durable.AppendEventResult{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(op); err != nil {
		return durable.AppendEventResult{}, err
	}
	session, exists := store.sessions[params.SessionID]
	if !exists {
		return durable.AppendEventResult{}, notFound(op, "session")
	}
	if session.State.Terminal() {
		return durable.AppendEventResult{}, durable.NewError(durable.CodeInvalidState, op, "terminal session cannot accept events", nil)
	}
	generation, err := store.generation(op, params.SessionID, params.Generation)
	if err != nil {
		return durable.AppendEventResult{}, err
	}
	if generation.State.Terminal() {
		return durable.AppendEventResult{}, durable.NewError(durable.CodeInvalidState, op, "terminal generation cannot accept events", nil)
	}

	event := eventFromParams(params, session.LastSequence+1)
	if existing, exists := store.eventByID[params.EventID]; exists {
		if !eventsEqual(existing, event, false) {
			return durable.AppendEventResult{}, durable.NewError(durable.CodeImmutableConflict, op, "event ID is already bound to different content", nil)
		}
		return durable.AppendEventResult{Event: cloneEvent(existing), Created: false}, nil
	}

	store.events[params.SessionID] = append(store.events[params.SessionID], event)
	store.eventByID[event.EventID] = event
	session.LastSequence = event.Sequence
	if event.Timestamp.After(session.UpdatedAt) {
		session.UpdatedAt = event.Timestamp
	}
	store.sessions[session.ID] = session
	return durable.AppendEventResult{Event: cloneEvent(event), Created: true}, nil
}

func (store *Store) ListEvents(ctx context.Context, query durable.EventQuery) (durable.EventPage, error) {
	const op = "list_events"
	if err := checkContext(ctx); err != nil {
		return durable.EventPage{}, err
	}
	if query.SessionID == "" || query.AfterSequence < 0 || query.Limit < 0 {
		return durable.EventPage{}, durable.NewError(durable.CodeInvalidArgument, op, "invalid event query", nil)
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(op); err != nil {
		return durable.EventPage{}, err
	}
	session, exists := store.sessions[query.SessionID]
	if !exists {
		return durable.EventPage{}, notFound(op, "session")
	}
	if query.AfterSequence > session.LastSequence {
		return durable.EventPage{}, durable.NewError(durable.CodeInvalidCursor, op, "cursor is beyond the durable tail", nil)
	}

	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	all := store.events[query.SessionID]
	start := int(query.AfterSequence)
	end := min(start+limit, len(all))
	pageEvents := make([]durable.Event, 0, end-start)
	for _, event := range all[start:end] {
		pageEvents = append(pageEvents, cloneEvent(event))
	}
	return durable.EventPage{
		Events:       pageEvents,
		LastSequence: session.LastSequence,
		HasMore:      end < len(all),
	}, nil
}

func validateAppendEvent(params durable.AppendEventParams) error {
	const op = "append_event"
	switch {
	case params.SchemaVersion == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "schema version is required", nil)
	case params.EventID == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "event ID is required", nil)
	case params.SessionID == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "session ID is required", nil)
	case params.Generation < 1:
		return durable.NewError(durable.CodeInvalidArgument, op, "generation must be positive", nil)
	case params.Timestamp.IsZero():
		return durable.NewError(durable.CodeInvalidArgument, op, "timestamp is required", nil)
	case params.Type == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "event type is required", nil)
	case params.Stream == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "event stream is required", nil)
	case len(params.Payload) > 0 && !json.Valid(params.Payload):
		return durable.NewError(durable.CodeInvalidArgument, op, "payload must be valid JSON", nil)
	default:
		return nil
	}
}

func eventFromParams(params durable.AppendEventParams, sequence int64) durable.Event {
	raw := append([]byte(nil), params.Raw...)
	digest := sha256.Sum256(raw)
	return durable.Event{
		SchemaVersion: params.SchemaVersion,
		EventID:       params.EventID,
		SessionID:     params.SessionID,
		Generation:    params.Generation,
		Sequence:      sequence,
		Timestamp:     normalizedTime(params.Timestamp),
		Type:          params.Type,
		Stream:        params.Stream,
		Payload:       append(json.RawMessage(nil), params.Payload...),
		Raw:           raw,
		RawSHA256:     "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func cloneEvent(event durable.Event) durable.Event {
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	event.Raw = append([]byte(nil), event.Raw...)
	return event
}

func eventsEqual(left, right durable.Event, includeSequence bool) bool {
	if left.SchemaVersion != right.SchemaVersion || left.EventID != right.EventID ||
		left.SessionID != right.SessionID || left.Generation != right.Generation ||
		!left.Timestamp.Equal(right.Timestamp) || left.Type != right.Type ||
		left.Stream != right.Stream || left.RawSHA256 != right.RawSHA256 ||
		!bytes.Equal(left.Payload, right.Payload) || !bytes.Equal(left.Raw, right.Raw) {
		return false
	}
	return !includeSequence || left.Sequence == right.Sequence
}
