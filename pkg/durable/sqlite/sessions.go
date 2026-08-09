package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

const sessionColumns = `id, idempotency_key, request_hash, request_manifest_json,
secret_grants_json, agent, runtime, state, active_generation, last_sequence,
created_at_ns, updated_at_ns`

type rowScanner interface {
	Scan(...any) error
}

func (store *Store) CreateSession(ctx context.Context, params durable.CreateSessionParams) (durable.CreateSessionResult, error) {
	const op = "create_session"
	if err := validateCreateSession(params); err != nil {
		return durable.CreateSessionResult{}, err
	}
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.CreateSessionResult{}, err
	}
	defer rollback(tx)

	existing, err := scanSession(tx.QueryRowContext(ctx,
		"SELECT "+sessionColumns+" FROM sessions WHERE idempotency_key = ?", params.IdempotencyKey))
	if err == nil {
		if existing.RequestHash != params.RequestHash {
			return durable.CreateSessionResult{}, durable.NewError(durable.CodeIdempotencyConflict, op, "idempotency key is already bound to a different request hash", nil)
		}
		return durable.CreateSessionResult{Session: existing, Created: false}, nil
	}
	if !durable.IsCode(err, durable.CodeNotFound) {
		return durable.CreateSessionResult{}, err
	}
	if _, err := scanSession(tx.QueryRowContext(ctx,
		"SELECT "+sessionColumns+" FROM sessions WHERE id = ?", params.SessionID)); err == nil {
		return durable.CreateSessionResult{}, durable.NewError(durable.CodeImmutableConflict, op, "session ID is already bound to another idempotency key", nil)
	} else if !durable.IsCode(err, durable.CodeNotFound) {
		return durable.CreateSessionResult{}, err
	}

	createdAt := normalizedTime(params.CreatedAt)
	grants, err := json.Marshal(params.SecretGrants)
	if err != nil {
		return durable.CreateSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "encode secret grants", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions (
id, idempotency_key, request_hash, request_manifest_json, secret_grants_json,
agent, runtime, state, active_generation, last_sequence, created_at_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		params.SessionID, params.IdempotencyKey, params.RequestHash, []byte(params.RequestManifest), grants,
		params.Agent, params.Runtime, durable.StateCreated, createdAt.UnixNano(), createdAt.UnixNano())
	if err != nil {
		return durable.CreateSessionResult{}, storageError(op, "insert session", err)
	}
	if err := tx.Commit(); err != nil {
		return durable.CreateSessionResult{}, storageError(op, "commit session", err)
	}
	return durable.CreateSessionResult{Session: durable.Session{
		ID: params.SessionID, IdempotencyKey: params.IdempotencyKey, RequestHash: params.RequestHash,
		RequestManifest: append(json.RawMessage(nil), params.RequestManifest...),
		SecretGrants:    append([]string(nil), params.SecretGrants...), Agent: params.Agent, Runtime: params.Runtime,
		State: durable.StateCreated, CreatedAt: createdAt, UpdatedAt: createdAt,
	}, Created: true}, nil
}

func (store *Store) GetSession(ctx context.Context, sessionID string) (durable.Session, error) {
	return store.getSession(ctx, "get_session", "id", sessionID)
}

func (store *Store) GetSessionByIdempotencyKey(ctx context.Context, key string) (durable.Session, error) {
	return store.getSession(ctx, "get_session_by_idempotency_key", "idempotency_key", key)
}

func (store *Store) getSession(ctx context.Context, op, column, value string) (durable.Session, error) {
	if value == "" {
		return durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "lookup value is required", nil)
	}
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.Session{}, err
	}
	defer rollback(tx)
	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE "+column+" = ?", value))
	if err != nil {
		return durable.Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return durable.Session{}, storageError(op, "commit session read", err)
	}
	return session, nil
}

func (store *Store) TransitionSession(ctx context.Context, params durable.TransitionSessionParams) (durable.Session, error) {
	const op = "transition_session"
	if params.SessionID == "" || params.From == "" || params.To == "" {
		return durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "session ID, from, and to are required", nil)
	}
	if !validSessionTransition(params.From, params.To) {
		return durable.Session{}, durable.NewError(durable.CodeInvalidState, op, "session transition is not allowed", nil)
	}
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.Session{}, err
	}
	defer rollback(tx)
	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id = ?", params.SessionID))
	if err != nil {
		return durable.Session{}, err
	}
	if session.State != params.From {
		return durable.Session{}, durable.NewError(durable.CodeInvalidState, op, "session is not in the expected state", nil)
	}
	at := normalizedTime(params.At)
	if at.Before(session.UpdatedAt) {
		at = session.UpdatedAt
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET state = ?, updated_at_ns = ? WHERE id = ?", params.To, at.UnixNano(), params.SessionID); err != nil {
		return durable.Session{}, storageError(op, "update session", err)
	}
	if err := tx.Commit(); err != nil {
		return durable.Session{}, storageError(op, "commit session transition", err)
	}
	session.State, session.UpdatedAt = params.To, at
	return session, nil
}

func scanSession(row rowScanner) (durable.Session, error) {
	const op = "scan_session"
	var session durable.Session
	var manifest, grants []byte
	var state string
	var createdAt, updatedAt int64
	err := row.Scan(&session.ID, &session.IdempotencyKey, &session.RequestHash, &manifest, &grants,
		&session.Agent, &session.Runtime, &state, &session.ActiveGeneration, &session.LastSequence,
		&createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return durable.Session{}, durable.NewError(durable.CodeNotFound, op, "session not found", nil)
		}
		return durable.Session{}, storageError(op, "scan session", err)
	}
	if err := json.Unmarshal(grants, &session.SecretGrants); err != nil {
		return durable.Session{}, durable.NewError(durable.CodeIndeterminate, op, "decode persisted secret grants", err)
	}
	session.RequestManifest = append(json.RawMessage(nil), manifest...)
	session.State = durable.SessionState(state)
	session.CreatedAt, session.UpdatedAt = timeFromUnixNano(createdAt), timeFromUnixNano(updatedAt)
	return session, nil
}

func validateCreateSession(params durable.CreateSessionParams) error {
	const op = "create_session"
	switch {
	case params.SessionID == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "session ID is required", nil)
	case params.IdempotencyKey == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "idempotency key is required", nil)
	case params.RequestHash == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "request hash is required", nil)
	case len(params.RequestManifest) == 0 || !json.Valid(params.RequestManifest):
		return durable.NewError(durable.CodeInvalidArgument, op, "request manifest must be valid JSON", nil)
	case params.Agent == "" || params.Runtime == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "agent and runtime are required", nil)
	default:
		return nil
	}
}

func validSessionTransition(from, to durable.SessionState) bool {
	if from.Terminal() {
		return false
	}
	switch from {
	case durable.StateCreated:
		return to == durable.StateStarting
	case durable.StateStarting:
		return to == durable.StateRunning
	default:
		return false
	}
}

func normalizedTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC().Round(0)
	}
	return value.UTC().Round(0)
}

func timeFromUnixNano(value int64) time.Time {
	return time.Unix(0, value).UTC()
}
