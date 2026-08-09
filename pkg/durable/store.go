package durable

import "context"

// SessionRepository owns logical identity and lifecycle transitions.
type SessionRepository interface {
	CreateSession(context.Context, CreateSessionParams) (CreateSessionResult, error)
	GetSession(context.Context, string) (Session, error)
	GetSessionByIdempotencyKey(context.Context, string) (Session, error)
	ListSessions(context.Context) ([]Session, error)
	TransitionSession(context.Context, TransitionSessionParams) (Session, error)
	FinalizeSession(context.Context, FinalizeSessionParams) (FinalizeSessionResult, error)
}

// GenerationRepository owns concrete runtime incarnation identity and state.
type GenerationRepository interface {
	CreateGeneration(context.Context, CreateGenerationParams) (Generation, error)
	BindGenerationProvider(context.Context, BindGenerationProviderParams) (Generation, error)
	GetGeneration(context.Context, string, int64) (Generation, error)
	ListGenerations(context.Context, string) ([]Generation, error)
	TransitionGeneration(context.Context, TransitionGenerationParams) (Generation, error)
}

// EventRepository owns atomic sequence allocation and immutable replay records.
type EventRepository interface {
	AppendEvent(context.Context, AppendEventParams) (AppendEventResult, error)
	ListEvents(context.Context, EventQuery) (EventPage, error)
}

// ReceiptRepository reads immutable terminal proof. Receipt creation occurs
// only through SessionRepository.FinalizeSession so state and proof are atomic.
type ReceiptRepository interface {
	GetTerminalReceipt(context.Context, string) (TerminalReceipt, error)
}

// Store is the single canonical DUR-101 persistence entry point.
// Implementations must provide identical transaction and immutability semantics.
type Store interface {
	SessionRepository
	GenerationRepository
	EventRepository
	ReceiptRepository
	Close() error
}
