package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/danieliser/agentruntime/pkg/durable"
)

const eventColumns = `session_id, sequence, event_id, generation, schema_version,
timestamp_ns, type, stream, payload_json, raw, raw_sha256`

func (store *Store) AppendEvent(ctx context.Context, params durable.AppendEventParams) (durable.AppendEventResult, error) {
	const op = "append_event"
	if err := validateAppendEvent(params); err != nil {
		return durable.AppendEventResult{}, err
	}
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.AppendEventResult{}, err
	}
	defer rollback(tx)

	digest := sha256.Sum256(params.Raw)
	candidate := eventFromParams(params, 0, "sha256:"+hex.EncodeToString(digest[:]))
	existing, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM events WHERE event_id = ?", params.EventID))
	if err == nil {
		if !eventsEqual(existing, candidate, false) {
			return durable.AppendEventResult{}, durable.NewError(durable.CodeImmutableConflict, op, "event ID is already bound to different content", nil)
		}
		return durable.AppendEventResult{Event: existing, Created: false}, nil
	}
	if !durable.IsCode(err, durable.CodeNotFound) {
		return durable.AppendEventResult{}, err
	}

	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id = ?", params.SessionID))
	if err != nil {
		return durable.AppendEventResult{}, err
	}
	if session.State.Terminal() {
		return durable.AppendEventResult{}, durable.NewError(durable.CodeInvalidState, op, "terminal session cannot accept events", nil)
	}
	generation, err := scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM runtime_generations WHERE session_id = ? AND generation = ?", params.SessionID, params.Generation))
	if err != nil {
		return durable.AppendEventResult{}, err
	}
	if generation.State.Terminal() {
		return durable.AppendEventResult{}, durable.NewError(durable.CodeInvalidState, op, "terminal generation cannot accept events", nil)
	}

	candidate.Sequence = session.LastSequence + 1
	var payload any
	if len(candidate.Payload) > 0 {
		payload = string(candidate.Payload)
	}
	raw := append([]byte{}, candidate.Raw...)
	_, err = tx.ExecContext(ctx, `INSERT INTO events (
session_id, sequence, event_id, generation, schema_version, timestamp_ns,
type, stream, payload_json, raw, raw_sha256
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.SessionID, candidate.Sequence, candidate.EventID, candidate.Generation,
		candidate.SchemaVersion, candidate.Timestamp.UnixNano(), candidate.Type, candidate.Stream,
		payload, raw, candidate.RawSHA256)
	if err != nil {
		return durable.AppendEventResult{}, storageError(op, "insert event", err)
	}
	updatedAt := candidate.Timestamp
	if updatedAt.Before(session.UpdatedAt) {
		updatedAt = session.UpdatedAt
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET last_sequence = ?, updated_at_ns = ? WHERE id = ?", candidate.Sequence, updatedAt.UnixNano(), params.SessionID); err != nil {
		return durable.AppendEventResult{}, storageError(op, "advance event tail", err)
	}
	if err := tx.Commit(); err != nil {
		return durable.AppendEventResult{}, storageError(op, "commit event", err)
	}
	return durable.AppendEventResult{Event: candidate, Created: true}, nil
}

func (store *Store) ListEvents(ctx context.Context, query durable.EventQuery) (durable.EventPage, error) {
	const op = "list_events"
	if query.SessionID == "" || query.AfterSequence < 0 || query.Limit < 0 {
		return durable.EventPage{}, durable.NewError(durable.CodeInvalidArgument, op, "invalid event query", nil)
	}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.EventPage{}, err
	}
	defer rollback(tx)
	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id = ?", query.SessionID))
	if err != nil {
		return durable.EventPage{}, err
	}
	if query.AfterSequence > session.LastSequence {
		return durable.EventPage{}, durable.NewError(durable.CodeInvalidCursor, op, "cursor is beyond the durable tail", nil)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events WHERE session_id = ? AND sequence > ? ORDER BY sequence LIMIT ?", query.SessionID, query.AfterSequence, limit)
	if err != nil {
		return durable.EventPage{}, storageError(op, "query events", err)
	}
	events := make([]durable.Event, 0, limit)
	expected := query.AfterSequence + 1
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			_ = rows.Close()
			return durable.EventPage{}, err
		}
		if event.Sequence != expected {
			_ = rows.Close()
			return durable.EventPage{}, durable.NewError(durable.CodeEventGap, op, "stored event sequence is not contiguous", nil)
		}
		events = append(events, event)
		expected++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return durable.EventPage{}, storageError(op, "iterate events", err)
	}
	if err := rows.Close(); err != nil {
		return durable.EventPage{}, storageError(op, "close event rows", err)
	}
	if expected <= session.LastSequence && len(events) < limit {
		return durable.EventPage{}, durable.NewError(durable.CodeEventGap, op, "durable tail contains a missing event range", nil)
	}
	if err := tx.Commit(); err != nil {
		return durable.EventPage{}, storageError(op, "commit event list", err)
	}
	earliest := int64(0)
	if session.LastSequence > 0 {
		earliest = 1
	}
	return durable.EventPage{
		Events: events, EarliestSequence: earliest,
		LastSequence: session.LastSequence, HasMore: expected <= session.LastSequence,
	}, nil
}

func scanEvent(row rowScanner) (durable.Event, error) {
	const op = "scan_event"
	var event durable.Event
	var stream string
	var timestamp int64
	var payload []byte
	err := row.Scan(&event.SessionID, &event.Sequence, &event.EventID, &event.Generation,
		&event.SchemaVersion, &timestamp, &event.Type, &stream, &payload, &event.Raw, &event.RawSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return durable.Event{}, durable.NewError(durable.CodeNotFound, op, "event not found", nil)
	}
	if err != nil {
		return durable.Event{}, storageError(op, "scan event", err)
	}
	event.Timestamp = timeFromUnixNano(timestamp)
	event.Stream = durable.Stream(stream)
	event.Payload = append(json.RawMessage(nil), payload...)
	event.Raw = append([]byte(nil), event.Raw...)
	return event, nil
}

func validateAppendEvent(params durable.AppendEventParams) error {
	const op = "append_event"
	switch {
	case params.SchemaVersion == "" || params.EventID == "" || params.SessionID == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "schema version, event ID, and session ID are required", nil)
	case params.Generation < 1 || params.Timestamp.IsZero():
		return durable.NewError(durable.CodeInvalidArgument, op, "generation and timestamp are required", nil)
	case params.Type == "" || params.Stream == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "event type and stream are required", nil)
	case len(params.Payload) > 0 && !json.Valid(params.Payload):
		return durable.NewError(durable.CodeInvalidArgument, op, "payload must be valid JSON", nil)
	default:
		return nil
	}
}

func eventFromParams(params durable.AppendEventParams, sequence int64, rawHash string) durable.Event {
	return durable.Event{
		SchemaVersion: params.SchemaVersion, EventID: params.EventID, SessionID: params.SessionID,
		Generation: params.Generation, Sequence: sequence, Timestamp: normalizedTime(params.Timestamp),
		Type: params.Type, Stream: params.Stream, Payload: append(json.RawMessage(nil), params.Payload...),
		Raw: append([]byte(nil), params.Raw...), RawSHA256: rawHash,
	}
}

func eventsEqual(left, right durable.Event, includeSequence bool) bool {
	// A recovered log prefix is observed at a new wall-clock time and may be
	// re-derived by newer code. Source identity plus exact raw bytes determine
	// idempotency; the first committed envelope remains authoritative.
	if left.EventID != right.EventID || left.SessionID != right.SessionID ||
		left.Generation != right.Generation || left.Stream != right.Stream ||
		left.RawSHA256 != right.RawSHA256 || !bytes.Equal(left.Raw, right.Raw) {
		return false
	}
	return !includeSequence || left.Sequence == right.Sequence
}
