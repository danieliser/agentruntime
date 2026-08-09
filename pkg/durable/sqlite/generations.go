package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/danieliser/agentruntime/pkg/durable"
)

const generationColumns = `session_id, generation, runtime, state, container_id,
image_reference, image_digest, sandbox_profile, provider_id, docker_log_driver,
docker_log_options_json, created_at_ns, updated_at_ns`

func (store *Store) CreateGeneration(ctx context.Context, params durable.CreateGenerationParams) (durable.Generation, error) {
	const op = "create_generation"
	if params.SessionID == "" || params.Runtime == "" || params.ContainerID == "" {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidArgument, op, "session ID, runtime, and container ID are required", nil)
	}
	logOptions := params.DockerLogOptions
	if len(logOptions) == 0 {
		logOptions = json.RawMessage(`{}`)
	}
	if !json.Valid(logOptions) {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidArgument, op, "Docker log options must be valid JSON", nil)
	}
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.Generation{}, err
	}
	defer rollback(tx)
	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id = ?", params.SessionID))
	if err != nil {
		return durable.Generation{}, err
	}
	if session.State.Terminal() {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "terminal session cannot start a generation", nil)
	}
	if session.ActiveGeneration > 0 {
		current, err := scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM runtime_generations WHERE session_id = ? AND generation = ?", params.SessionID, session.ActiveGeneration))
		if err != nil {
			return durable.Generation{}, err
		}
		if !current.State.Terminal() {
			return durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "previous generation is still active", nil)
		}
	}
	var existingContainer string
	err = tx.QueryRowContext(ctx, "SELECT container_id FROM runtime_generations WHERE container_id = ?", params.ContainerID).Scan(&existingContainer)
	if err == nil {
		return durable.Generation{}, durable.NewError(durable.CodeImmutableConflict, op, "container ID already belongs to a generation", nil)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return durable.Generation{}, storageError(op, "check container identity", err)
	}

	createdAt := normalizedTime(params.CreatedAt)
	number := session.ActiveGeneration + 1
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_generations (
session_id, generation, runtime, state, container_id, image_reference, image_digest,
sandbox_profile, provider_id, docker_log_driver, docker_log_options_json, created_at_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		params.SessionID, number, params.Runtime, durable.GenerationStarting, params.ContainerID,
		params.ImageReference, params.ImageDigest, params.SandboxProfile, params.ProviderID,
		params.DockerLogDriver, []byte(logOptions), createdAt.UnixNano(), createdAt.UnixNano())
	if err != nil {
		return durable.Generation{}, storageError(op, "insert generation", err)
	}
	updatedAt := createdAt
	if updatedAt.Before(session.UpdatedAt) {
		updatedAt = session.UpdatedAt
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET active_generation = ?, updated_at_ns = ? WHERE id = ?", number, updatedAt.UnixNano(), params.SessionID); err != nil {
		return durable.Generation{}, storageError(op, "activate generation", err)
	}
	if err := tx.Commit(); err != nil {
		return durable.Generation{}, storageError(op, "commit generation", err)
	}
	return durable.Generation{
		SessionID: params.SessionID, Number: number, Runtime: params.Runtime, State: durable.GenerationStarting,
		ContainerID: params.ContainerID, ImageReference: params.ImageReference, ImageDigest: params.ImageDigest,
		SandboxProfile: params.SandboxProfile, ProviderID: params.ProviderID,
		DockerLogDriver: params.DockerLogDriver, DockerLogOptions: append(json.RawMessage(nil), logOptions...),
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}, nil
}

func (store *Store) GetGeneration(ctx context.Context, sessionID string, number int64) (durable.Generation, error) {
	const op = "get_generation"
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.Generation{}, err
	}
	defer rollback(tx)
	generation, err := scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM runtime_generations WHERE session_id = ? AND generation = ?", sessionID, number))
	if err != nil {
		return durable.Generation{}, err
	}
	if err := tx.Commit(); err != nil {
		return durable.Generation{}, storageError(op, "commit generation read", err)
	}
	return generation, nil
}

func (store *Store) ListGenerations(ctx context.Context, sessionID string) ([]durable.Generation, error) {
	const op = "list_generations"
	tx, err := store.begin(ctx, op)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	if _, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id = ?", sessionID)); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+generationColumns+" FROM runtime_generations WHERE session_id = ? ORDER BY generation", sessionID)
	if err != nil {
		return nil, storageError(op, "query generations", err)
	}
	defer rows.Close()
	var generations []durable.Generation
	for rows.Next() {
		generation, err := scanGeneration(rows)
		if err != nil {
			return nil, err
		}
		generations = append(generations, generation)
	}
	if err := rows.Err(); err != nil {
		return nil, storageError(op, "iterate generations", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, storageError(op, "commit generation list", err)
	}
	return generations, nil
}

func (store *Store) TransitionGeneration(ctx context.Context, params durable.TransitionGenerationParams) (durable.Generation, error) {
	const op = "transition_generation"
	if params.SessionID == "" || params.Generation < 1 || params.From == "" || params.To == "" {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidArgument, op, "session, generation, from, and to are required", nil)
	}
	if !validGenerationTransition(params.From, params.To) {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "generation transition is not allowed", nil)
	}
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.Generation{}, err
	}
	defer rollback(tx)
	generation, err := scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM runtime_generations WHERE session_id = ? AND generation = ?", params.SessionID, params.Generation))
	if err != nil {
		return durable.Generation{}, err
	}
	if generation.State != params.From {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "generation is not in the expected state", nil)
	}
	at := normalizedTime(params.At)
	if at.Before(generation.UpdatedAt) {
		at = generation.UpdatedAt
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runtime_generations SET state = ?, updated_at_ns = ? WHERE session_id = ? AND generation = ?", params.To, at.UnixNano(), params.SessionID, params.Generation); err != nil {
		return durable.Generation{}, storageError(op, "update generation", err)
	}
	if err := tx.Commit(); err != nil {
		return durable.Generation{}, storageError(op, "commit generation transition", err)
	}
	generation.State, generation.UpdatedAt = params.To, at
	return generation, nil
}

func scanGeneration(row rowScanner) (durable.Generation, error) {
	const op = "scan_generation"
	var generation durable.Generation
	var state string
	var logOptions []byte
	var createdAt, updatedAt int64
	err := row.Scan(&generation.SessionID, &generation.Number, &generation.Runtime, &state,
		&generation.ContainerID, &generation.ImageReference, &generation.ImageDigest,
		&generation.SandboxProfile, &generation.ProviderID, &generation.DockerLogDriver,
		&logOptions, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return durable.Generation{}, durable.NewError(durable.CodeNotFound, op, "generation not found", nil)
	}
	if err != nil {
		return durable.Generation{}, storageError(op, "scan generation", err)
	}
	generation.State = durable.GenerationState(state)
	generation.DockerLogOptions = append(json.RawMessage(nil), logOptions...)
	generation.CreatedAt, generation.UpdatedAt = timeFromUnixNano(createdAt), timeFromUnixNano(updatedAt)
	return generation, nil
}

func validGenerationTransition(from, to durable.GenerationState) bool {
	if from.Terminal() {
		return false
	}
	switch from {
	case durable.GenerationStarting:
		return to == durable.GenerationRunning || to.Terminal()
	case durable.GenerationRunning:
		return to.Terminal()
	default:
		return false
	}
}
