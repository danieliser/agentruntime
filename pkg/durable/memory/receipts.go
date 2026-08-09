package memory

import (
	"context"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// FinalizeSession implements DUR-105 atomic terminal state and receipt commit.
func (store *Store) FinalizeSession(ctx context.Context, params durable.FinalizeSessionParams) (durable.FinalizeSessionResult, error) {
	const op = "finalize_session"
	if err := checkContext(ctx); err != nil {
		return durable.FinalizeSessionResult{}, err
	}
	receipt := params.Receipt
	if receipt.SessionID == "" || receipt.Generation < 1 || !receipt.State.Terminal() || receipt.StartedAt.IsZero() || receipt.EndedAt.IsZero() {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "receipt requires session, generation, terminal state, start time, and end time", nil)
	}
	if params.From.Terminal() || params.GenerationFrom.Terminal() || !params.GenerationTo.Terminal() {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "finalization requires nonterminal source and terminal generation target", nil)
	}
	if !validFinalTransition(params.From, receipt.State) {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidState, op, "terminal session transition is not allowed", nil)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(op); err != nil {
		return durable.FinalizeSessionResult{}, err
	}
	session, exists := store.sessions[receipt.SessionID]
	if !exists {
		return durable.FinalizeSessionResult{}, notFound(op, "session")
	}
	generation, err := store.generation(op, receipt.SessionID, receipt.Generation)
	if err != nil {
		return durable.FinalizeSessionResult{}, err
	}

	receipt.StartedAt = normalizedTime(receipt.StartedAt)
	receipt.EndedAt = normalizedTime(receipt.EndedAt)
	if receipt.EndedAt.Before(receipt.StartedAt) {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "receipt end time precedes start time", nil)
	}
	if existing, exists := store.receipts[receipt.SessionID]; exists {
		if session.State != receipt.State || generation.State != params.GenerationTo || !receiptsEqual(existing, receipt) {
			return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeImmutableConflict, op, "terminal receipt is immutable", nil)
		}
		return durable.FinalizeSessionResult{Session: cloneSession(session), Receipt: cloneReceipt(existing), Created: false}, nil
	}
	if session.State != params.From || generation.State != params.GenerationFrom {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidState, op, "session or generation is not in the expected state", nil)
	}
	if session.LastSequence != receipt.LastSequence {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidState, op, "receipt last sequence does not match the session", nil)
	}

	generation.State = params.GenerationTo
	generation.UpdatedAt = receipt.EndedAt
	store.generations[receipt.SessionID][receipt.Generation-1] = generation
	session.State = receipt.State
	session.UpdatedAt = receipt.EndedAt
	store.sessions[receipt.SessionID] = session
	store.receipts[receipt.SessionID] = cloneReceipt(receipt)
	return durable.FinalizeSessionResult{Session: cloneSession(session), Receipt: cloneReceipt(receipt), Created: true}, nil
}

func validFinalTransition(from, to durable.SessionState) bool {
	switch from {
	case durable.StateCreated:
		return to == durable.StateFailed || to == durable.StateCancelled || to == durable.StateIndeterminate
	case durable.StateStarting:
		return to == durable.StateFailed || to == durable.StateCancelled || to == durable.StateTimedOut || to == durable.StateCrashed || to == durable.StateIndeterminate
	case durable.StateRunning:
		return to.Terminal()
	default:
		return false
	}
}

func (store *Store) GetTerminalReceipt(ctx context.Context, sessionID string) (durable.TerminalReceipt, error) {
	const op = "get_terminal_receipt"
	if err := checkContext(ctx); err != nil {
		return durable.TerminalReceipt{}, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(op); err != nil {
		return durable.TerminalReceipt{}, err
	}
	receipt, exists := store.receipts[sessionID]
	if !exists {
		return durable.TerminalReceipt{}, notFound(op, "terminal receipt")
	}
	return cloneReceipt(receipt), nil
}

func receiptsEqual(left, right durable.TerminalReceipt) bool {
	if left.SessionID != right.SessionID || left.Generation != right.Generation ||
		left.State != right.State || left.Signal != right.Signal ||
		!left.StartedAt.Equal(right.StartedAt) || !left.EndedAt.Equal(right.EndedAt) ||
		left.OutputHash != right.OutputHash || left.ArtifactHash != right.ArtifactHash ||
		left.LastSequence != right.LastSequence {
		return false
	}
	if left.ExitCode == nil || right.ExitCode == nil {
		return left.ExitCode == nil && right.ExitCode == nil
	}
	return *left.ExitCode == *right.ExitCode
}

func cloneReceipt(receipt durable.TerminalReceipt) durable.TerminalReceipt {
	if receipt.ExitCode != nil {
		exitCode := *receipt.ExitCode
		receipt.ExitCode = &exitCode
	}
	return receipt
}
