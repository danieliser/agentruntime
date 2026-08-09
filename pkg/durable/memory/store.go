// Package memory provides the DUR-101 through DUR-105 reference implementation
// of the durable Store contract. It is an executable specification, not a
// recovery authority for production AgentD sessions.
package memory

import (
	"context"
	"sync"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// Store implements durable.Store with process-local immutable copies.
type Store struct {
	mu sync.RWMutex

	closed bool

	sessions     map[string]durable.Session
	sessionByKey map[string]string
	generations  map[string][]durable.Generation
	containerIDs map[string]generationKey
	events       map[string][]durable.Event
	eventByID    map[string]durable.Event
	receipts     map[string]durable.TerminalReceipt
}

type generationKey struct {
	sessionID string
	number    int64
}

// New creates an empty reference store.
func New() *Store {
	return &Store{
		sessions:     make(map[string]durable.Session),
		sessionByKey: make(map[string]string),
		generations:  make(map[string][]durable.Generation),
		containerIDs: make(map[string]generationKey),
		events:       make(map[string][]durable.Event),
		eventByID:    make(map[string]durable.Event),
		receipts:     make(map[string]durable.TerminalReceipt),
	}
}

// Close is idempotent. Closed stores reject all later operations.
func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.closed = true
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return durable.NewError(durable.CodeInvalidArgument, "context", "context is nil", nil)
	}
	return ctx.Err()
}

func (store *Store) checkOpen(op string) error {
	if store.closed {
		return durable.NewError(durable.CodeStoreClosed, op, "store is closed", nil)
	}
	return nil
}

func notFound(op, resource string) error {
	return durable.NewError(durable.CodeNotFound, op, resource+" not found", nil)
}

var _ durable.Store = (*Store)(nil)
