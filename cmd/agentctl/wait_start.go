package main

import (
	"context"
	"errors"
	"time"

	agentclient "github.com/danieliser/agentruntime/pkg/client"
	"github.com/danieliser/agentruntime/pkg/durable"
)

type waitNotice struct {
	Sequence int64  `json:"sequence"`
	Type     string `json:"type"`
	Reason   string `json:"reason"`
	Summary  string `json:"summary,omitempty"`
}

// markControl records the session tail just before the caller sends a
// control, so the caller's next wait reacts only to what follows it. It is
// best effort: a failed mark never blocks the control. The returned undo
// restores the previous mark; call it when the control is rejected, so a
// control that never ran does not hide a finished session from wait.
func markControl(ctx *Context, session string) (undo func()) {
	undo = func() {}
	if ctx.Cursors == nil || ctx.API == nil {
		return undo
	}
	store, caller, key := *ctx.Cursors, ctx.Config.Caller, controlKey(session)
	prev, hadPrev, err := store.Lookup(caller, key)
	if err != nil {
		return undo
	}
	view, err := ctx.API.GetSession(context.Background(), session)
	if err != nil {
		return undo
	}
	if store.Save(caller, key, view.LastSequence) != nil {
		return undo
	}
	return func() {
		if hadPrev {
			_ = store.Save(caller, key, prev)
		} else {
			_ = store.Delete(caller, key)
		}
	}
}

// markCreated lets the creator's first wait read a new session from its start.
func markCreated(ctx *Context, session string) {
	if ctx.Cursors == nil {
		return
	}
	if _, found, err := ctx.Cursors.Lookup(ctx.Config.Caller, controlKey(session)); err != nil || found {
		return
	}
	_ = ctx.Cursors.Save(ctx.Config.Caller, controlKey(session), 0)
}

// waitStart decides where a session wait begins and saves it as the wait
// cursor. Saved wait progress or the caller's last control mark wins; a caller
// with neither starts at the current tail, so old events never match.
//
// Current state is reported at once instead of waiting for an event that
// already happened: a pending approval always, and a finished session unless
// the caller's newest control (which may be waking it) came after its last
// wait. The cursor is saved only after any notice is written, so a failed
// write never consumes the notice.
func waitStart(ctx context.Context, c *Context, session string, store CursorStore, reasons []string) (string, error) {
	caller, key := c.Config.Caller, waitKey(session)
	cursor, haveCursor, err := store.Lookup(caller, key)
	if err != nil {
		return "", err
	}
	floor, haveFloor, err := store.Lookup(caller, controlKey(session))
	if err != nil {
		return "", err
	}
	controlPending := haveFloor && (!haveCursor || floor > cursor)
	view, err := getSessionWithRetry(ctx, c, session)
	if err != nil {
		return "", err
	}
	start := view.LastSequence
	switch {
	case controlPending:
		start = floor
	case haveCursor:
		start = cursor
	}
	state := durable.SessionState(view.State)
	reason := ""
	switch {
	case state.Terminal() && !controlPending && contains(reasons, "done"):
		reason = "done"
	case state.Terminal() && !controlPending && state != durable.StateCompleted && contains(reasons, "error"):
		reason = "error"
	case !state.Terminal() && contains(reasons, "approval"):
		if list, e := approvals(c, session); e == nil && len(list.Approvals) > 0 {
			reason = "approval"
		}
	}
	if reason != "" {
		kind := "session." + view.State
		summary := "already " + view.State
		if reason == "approval" {
			kind, summary = "control.approval.request", "waiting for approval"
		}
		notice := waitNotice{Sequence: view.LastSequence, Type: kind, Reason: reason, Summary: summary}
		if err = WriteOutput(c.Out, c.Config, notice, kind+": "+summary); err != nil {
			return "", err
		}
		// A finished session is fully seen. A pending approval is current
		// state, so the stream position stays where it was.
		if reason != "approval" {
			start = view.LastSequence
		}
	}
	if !haveCursor || start != cursor {
		if err = store.Save(caller, key, start); err != nil {
			return "", err
		}
	}
	return reason, nil
}

func getSessionWithRetry(ctx context.Context, c *Context, session string) (*agentclient.DurableSession, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, lastErr
			case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
			}
		}
		view, err := c.API.GetSession(ctx, session)
		if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrAuth) {
			return view, err
		}
		lastErr = err
	}
	return nil, lastErr
}
