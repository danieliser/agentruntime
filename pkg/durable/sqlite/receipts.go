package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danieliser/agentruntime/pkg/durable"
)

const receiptColumns = `session_id, generation, state, terminal_reason, exit_code, signal,
started_at_ns, ended_at_ns, output_hash, artifact_hash, last_sequence`

func (store *Store) FinalizeSession(ctx context.Context, params durable.FinalizeSessionParams) (durable.FinalizeSessionResult, error) {
	const op = "finalize_session"
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
	reason, valid := durable.NormalizeTerminalReason(receipt.State, receipt.Reason)
	if !valid {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "terminal reason does not match receipt state", nil)
	}
	receipt.Reason = reason
	receipt.StartedAt, receipt.EndedAt = normalizedTime(receipt.StartedAt), normalizedTime(receipt.EndedAt)
	if receipt.EndedAt.Before(receipt.StartedAt) {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "receipt end time precedes start time", nil)
	}

	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.FinalizeSessionResult{}, err
	}
	defer rollback(tx)
	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id = ?", receipt.SessionID))
	if err != nil {
		return durable.FinalizeSessionResult{}, err
	}
	generation, err := scanGeneration(tx.QueryRowContext(ctx, "SELECT "+generationColumns+" FROM runtime_generations WHERE session_id = ? AND generation = ?", receipt.SessionID, receipt.Generation))
	if err != nil {
		return durable.FinalizeSessionResult{}, err
	}
	existing, err := scanReceipt(tx.QueryRowContext(ctx, "SELECT "+receiptColumns+" FROM terminal_receipts WHERE session_id = ?", receipt.SessionID))
	if err == nil {
		if session.State != receipt.State || generation.State != params.GenerationTo || !receiptsEqual(existing, receipt) {
			return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeImmutableConflict, op, "terminal receipt is immutable", nil)
		}
		return durable.FinalizeSessionResult{Session: session, Receipt: existing, Created: false}, nil
	}
	if !durable.IsCode(err, durable.CodeNotFound) {
		return durable.FinalizeSessionResult{}, err
	}
	if session.State != params.From || generation.State != params.GenerationFrom {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidState, op, "session or generation is not in the expected state", nil)
	}
	if session.LastSequence != receipt.LastSequence {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidState, op, "receipt last sequence does not match the session", nil)
	}
	if receipt.EndedAt.Before(session.UpdatedAt) || receipt.EndedAt.Before(generation.UpdatedAt) {
		return durable.FinalizeSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "receipt end time precedes durable session state", nil)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runtime_generations SET state = ?, updated_at_ns = ? WHERE session_id = ? AND generation = ?", params.GenerationTo, receipt.EndedAt.UnixNano(), receipt.SessionID, receipt.Generation); err != nil {
		return durable.FinalizeSessionResult{}, storageError(op, "finalize generation", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET state = ?, updated_at_ns = ? WHERE id = ?", receipt.State, receipt.EndedAt.UnixNano(), receipt.SessionID); err != nil {
		return durable.FinalizeSessionResult{}, storageError(op, "finalize session", err)
	}
	var exitCode any
	if receipt.ExitCode != nil {
		exitCode = *receipt.ExitCode
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO terminal_receipts (
session_id, generation, state, terminal_reason, exit_code, signal, started_at_ns, ended_at_ns,
output_hash, artifact_hash, last_sequence
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, receipt.SessionID, receipt.Generation, receipt.State, receipt.Reason,
		exitCode, receipt.Signal, receipt.StartedAt.UnixNano(), receipt.EndedAt.UnixNano(),
		receipt.OutputHash, receipt.ArtifactHash, receipt.LastSequence)
	if err != nil {
		return durable.FinalizeSessionResult{}, storageError(op, "insert terminal receipt", err)
	}
	if err := tx.Commit(); err != nil {
		return durable.FinalizeSessionResult{}, storageError(op, "commit terminal receipt", err)
	}
	session.State, session.UpdatedAt = receipt.State, receipt.EndedAt
	return durable.FinalizeSessionResult{Session: session, Receipt: cloneReceipt(receipt), Created: true}, nil
}

func (store *Store) GetTerminalReceipt(ctx context.Context, sessionID string) (durable.TerminalReceipt, error) {
	const op = "get_terminal_receipt"
	tx, err := store.begin(ctx, op)
	if err != nil {
		return durable.TerminalReceipt{}, err
	}
	defer rollback(tx)
	receipt, err := scanReceipt(tx.QueryRowContext(ctx, "SELECT "+receiptColumns+" FROM terminal_receipts WHERE session_id = ?", sessionID))
	if err != nil {
		return durable.TerminalReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return durable.TerminalReceipt{}, storageError(op, "commit receipt read", err)
	}
	return receipt, nil
}

func scanReceipt(row rowScanner) (durable.TerminalReceipt, error) {
	const op = "scan_terminal_receipt"
	var receipt durable.TerminalReceipt
	var state string
	var exitCode sql.NullInt64
	var startedAt, endedAt int64
	err := row.Scan(&receipt.SessionID, &receipt.Generation, &state, &receipt.Reason, &exitCode, &receipt.Signal,
		&startedAt, &endedAt, &receipt.OutputHash, &receipt.ArtifactHash, &receipt.LastSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return durable.TerminalReceipt{}, durable.NewError(durable.CodeNotFound, op, "terminal receipt not found", nil)
	}
	if err != nil {
		return durable.TerminalReceipt{}, storageError(op, "scan terminal receipt", err)
	}
	receipt.State = durable.SessionState(state)
	receipt.Reason, _ = durable.NormalizeTerminalReason(receipt.State, receipt.Reason)
	receipt.StartedAt, receipt.EndedAt = timeFromUnixNano(startedAt), timeFromUnixNano(endedAt)
	if exitCode.Valid {
		value := int(exitCode.Int64)
		receipt.ExitCode = &value
	}
	return receipt, nil
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

func receiptsEqual(left, right durable.TerminalReceipt) bool {
	if left.SessionID != right.SessionID || left.Generation != right.Generation || left.State != right.State ||
		left.Reason != right.Reason || left.Signal != right.Signal || !left.StartedAt.Equal(right.StartedAt) || !left.EndedAt.Equal(right.EndedAt) ||
		left.OutputHash != right.OutputHash || left.ArtifactHash != right.ArtifactHash || left.LastSequence != right.LastSequence {
		return false
	}
	if left.ExitCode == nil || right.ExitCode == nil {
		return left.ExitCode == nil && right.ExitCode == nil
	}
	return *left.ExitCode == *right.ExitCode
}

func cloneReceipt(receipt durable.TerminalReceipt) durable.TerminalReceipt {
	if receipt.ExitCode != nil {
		value := *receipt.ExitCode
		receipt.ExitCode = &value
	}
	return receipt
}
