package memory

import (
	"context"
	"encoding/json"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func (store *Store) CreateGeneration(ctx context.Context, params durable.CreateGenerationParams) (durable.Generation, error) {
	const op = "create_generation"
	if err := checkContext(ctx); err != nil {
		return durable.Generation{}, err
	}
	if params.SessionID == "" || params.Runtime == "" || params.ContainerID == "" {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidArgument, op, "session ID, runtime, and container ID are required", nil)
	}
	if len(params.DockerLogOptions) > 0 && !json.Valid(params.DockerLogOptions) {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidArgument, op, "Docker log options must be valid JSON", nil)
	}
	logOptions := params.DockerLogOptions
	if len(logOptions) == 0 {
		logOptions = json.RawMessage(`{}`)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(op); err != nil {
		return durable.Generation{}, err
	}
	session, exists := store.sessions[params.SessionID]
	if !exists {
		return durable.Generation{}, notFound(op, "session")
	}
	if session.State.Terminal() {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "terminal session cannot start a generation", nil)
	}
	if _, exists := store.containerIDs[params.ContainerID]; exists {
		return durable.Generation{}, durable.NewError(durable.CodeImmutableConflict, op, "container ID already belongs to a generation", nil)
	}
	existing := store.generations[params.SessionID]
	if len(existing) > 0 && !existing[len(existing)-1].State.Terminal() {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "previous generation is still active", nil)
	}

	createdAt := normalizedTime(params.CreatedAt)
	generation := durable.Generation{
		SessionID:        params.SessionID,
		Number:           int64(len(existing) + 1),
		Runtime:          params.Runtime,
		State:            durable.GenerationStarting,
		ContainerID:      params.ContainerID,
		ImageReference:   params.ImageReference,
		ImageDigest:      params.ImageDigest,
		SandboxProfile:   params.SandboxProfile,
		ProviderID:       params.ProviderID,
		DockerLogDriver:  params.DockerLogDriver,
		DockerLogOptions: append([]byte(nil), logOptions...),
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	}
	store.generations[params.SessionID] = append(existing, generation)
	store.containerIDs[params.ContainerID] = generationKey{sessionID: params.SessionID, number: generation.Number}
	session.ActiveGeneration = generation.Number
	session.UpdatedAt = createdAt
	store.sessions[session.ID] = session
	return cloneGeneration(generation), nil
}

func (store *Store) BindGenerationProvider(ctx context.Context, params durable.BindGenerationProviderParams) (durable.Generation, error) {
	const op = "bind_generation_provider"
	if err := checkContext(ctx); err != nil {
		return durable.Generation{}, err
	}
	if params.SessionID == "" || params.Generation < 1 || params.ProviderID == "" {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidArgument, op, "session, generation, and provider ID are required", nil)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(op); err != nil {
		return durable.Generation{}, err
	}
	generation, err := store.generation(op, params.SessionID, params.Generation)
	if err != nil {
		return durable.Generation{}, err
	}
	if generation.ProviderID == params.ProviderID {
		return cloneGeneration(generation), nil
	}
	if generation.ProviderID != "" {
		return durable.Generation{}, durable.NewError(durable.CodeImmutableConflict, op, "provider identity is already bound", nil)
	}
	generation.ProviderID = params.ProviderID
	at := normalizedTime(params.At)
	if at.Before(generation.UpdatedAt) {
		at = generation.UpdatedAt
	}
	generation.UpdatedAt = at
	store.generations[params.SessionID][params.Generation-1] = generation
	return cloneGeneration(generation), nil
}

func (store *Store) GetGeneration(ctx context.Context, sessionID string, number int64) (durable.Generation, error) {
	const op = "get_generation"
	if err := checkContext(ctx); err != nil {
		return durable.Generation{}, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(op); err != nil {
		return durable.Generation{}, err
	}
	generation, err := store.generation(op, sessionID, number)
	return cloneGeneration(generation), err
}

func (store *Store) ListGenerations(ctx context.Context, sessionID string) ([]durable.Generation, error) {
	const op = "list_generations"
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(op); err != nil {
		return nil, err
	}
	if _, exists := store.sessions[sessionID]; !exists {
		return nil, notFound(op, "session")
	}
	items := make([]durable.Generation, 0, len(store.generations[sessionID]))
	for _, generation := range store.generations[sessionID] {
		items = append(items, cloneGeneration(generation))
	}
	return items, nil
}

func (store *Store) TransitionGeneration(ctx context.Context, params durable.TransitionGenerationParams) (durable.Generation, error) {
	const op = "transition_generation"
	if err := checkContext(ctx); err != nil {
		return durable.Generation{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(op); err != nil {
		return durable.Generation{}, err
	}
	generation, err := store.generation(op, params.SessionID, params.Generation)
	if err != nil {
		return durable.Generation{}, err
	}
	if generation.State != params.From || !validGenerationTransition(params.From, params.To) {
		return durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "generation transition is not allowed", nil)
	}

	generation.State = params.To
	generation.UpdatedAt = normalizedTime(params.At)
	store.generations[params.SessionID][params.Generation-1] = generation
	return cloneGeneration(generation), nil
}

func cloneGeneration(generation durable.Generation) durable.Generation {
	generation.DockerLogOptions = append([]byte(nil), generation.DockerLogOptions...)
	return generation
}

func (store *Store) generation(op, sessionID string, number int64) (durable.Generation, error) {
	items := store.generations[sessionID]
	if number < 1 || number > int64(len(items)) {
		return durable.Generation{}, notFound(op, "generation")
	}
	return items[number-1], nil
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
