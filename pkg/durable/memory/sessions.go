package memory

import (
	"context"
	"encoding/json"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// CreateSession implements DUR-103 idempotent logical-session admission.
func (store *Store) CreateSession(ctx context.Context, params durable.CreateSessionParams) (durable.CreateSessionResult, error) {
	const op = "create_session"
	if err := checkContext(ctx); err != nil {
		return durable.CreateSessionResult{}, err
	}
	if err := validateCreateSession(params); err != nil {
		return durable.CreateSessionResult{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(op); err != nil {
		return durable.CreateSessionResult{}, err
	}

	if sessionID, exists := store.sessionByKey[params.IdempotencyKey]; exists {
		existing := store.sessions[sessionID]
		if existing.RequestHash != params.RequestHash {
			return durable.CreateSessionResult{}, durable.NewError(
				durable.CodeIdempotencyConflict,
				op,
				"idempotency key is already bound to a different request hash",
				nil,
			)
		}
		return durable.CreateSessionResult{Session: cloneSession(existing), Created: false}, nil
	}
	if _, exists := store.sessions[params.SessionID]; exists {
		return durable.CreateSessionResult{}, durable.NewError(
			durable.CodeImmutableConflict,
			op,
			"session ID is already bound to another idempotency key",
			nil,
		)
	}

	createdAt := normalizedTime(params.CreatedAt)
	session := durable.Session{
		ID:              params.SessionID,
		IdempotencyKey:  params.IdempotencyKey,
		RequestHash:     params.RequestHash,
		RequestManifest: append(json.RawMessage(nil), params.RequestManifest...),
		SecretGrants:    append([]string(nil), params.SecretGrants...),
		Agent:           params.Agent,
		Runtime:         params.Runtime,
		State:           durable.StateCreated,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	}
	store.sessions[session.ID] = session
	store.sessionByKey[session.IdempotencyKey] = session.ID
	return durable.CreateSessionResult{Session: cloneSession(session), Created: true}, nil
}

func (store *Store) GetSession(ctx context.Context, sessionID string) (durable.Session, error) {
	const op = "get_session"
	if err := checkContext(ctx); err != nil {
		return durable.Session{}, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(op); err != nil {
		return durable.Session{}, err
	}
	session, exists := store.sessions[sessionID]
	if !exists {
		return durable.Session{}, notFound(op, "session")
	}
	return cloneSession(session), nil
}

func (store *Store) GetSessionByIdempotencyKey(ctx context.Context, key string) (durable.Session, error) {
	const op = "get_session_by_idempotency_key"
	if err := checkContext(ctx); err != nil {
		return durable.Session{}, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(op); err != nil {
		return durable.Session{}, err
	}
	sessionID, exists := store.sessionByKey[key]
	if !exists {
		return durable.Session{}, notFound(op, "session")
	}
	return cloneSession(store.sessions[sessionID]), nil
}

func (store *Store) TransitionSession(ctx context.Context, params durable.TransitionSessionParams) (durable.Session, error) {
	const op = "transition_session"
	if err := checkContext(ctx); err != nil {
		return durable.Session{}, err
	}
	if params.SessionID == "" || params.From == "" || params.To == "" {
		return durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "session ID, from, and to are required", nil)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(op); err != nil {
		return durable.Session{}, err
	}
	session, exists := store.sessions[params.SessionID]
	if !exists {
		return durable.Session{}, notFound(op, "session")
	}
	if session.State != params.From {
		return durable.Session{}, durable.NewError(durable.CodeInvalidState, op, "session is not in the expected state", nil)
	}
	if !validSessionTransition(params.From, params.To) {
		return durable.Session{}, durable.NewError(durable.CodeInvalidState, op, "session transition is not allowed", nil)
	}

	session.State = params.To
	session.UpdatedAt = normalizedTime(params.At)
	store.sessions[session.ID] = session
	return cloneSession(session), nil
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
	case params.Agent == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "agent is required", nil)
	case params.Runtime == "":
		return durable.NewError(durable.CodeInvalidArgument, op, "runtime is required", nil)
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
		return time.Now().UTC()
	}
	return value.UTC().Round(0)
}

func cloneSession(session durable.Session) durable.Session {
	session.RequestManifest = append(json.RawMessage(nil), session.RequestManifest...)
	session.SecretGrants = append([]string(nil), session.SecretGrants...)
	return session
}
