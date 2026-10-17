package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func cmdStatus(ctx *Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: status ID", ErrUsage)
	}
	if ref, err := ParseSessionReference(args[0]); err == nil && ref.Kind == "plan" {
		return cmdPlan(ctx, []string{"show", ref.ID})
	}
	if ref, err := ParseSessionReference(args[0]); err == nil && ref.Kind == "plan_node" {
		id, err := ctx.Persist.resolve(ref)
		if err != nil {
			return err
		}
		args[0] = id
	}
	s, e := ctx.API.GetSession(context.Background(), args[0])
	if e != nil {
		return e
	}
	var a approvalList
	approvalsAvailable := true
	if s.State == "running" {
		a, e = approvals(ctx, args[0])
		if e != nil {
			var statusErr *apiStatusError
			if !errors.As(e, &statusErr) || statusErr.Status != http.StatusConflict {
				return e
			}
			approvalsAvailable = false
		}
	}
	var approvalCount any = len(a.Approvals)
	approvalText := fmt.Sprint(len(a.Approvals))
	if !approvalsAvailable {
		approvalCount = nil
		approvalText = "unavailable"
	}
	turn := "-"
	var cursor int64
	ordinal := 1
	for {
		page, pageErr := ctx.API.Events(context.Background(), args[0], cursor, 500)
		if pageErr != nil {
			return pageErr
		}
		for _, ev := range page.Events {
			cursor = ev.Sequence
			if strings.HasPrefix(ev.Type, "subagent.") {
				continue
			}
			var p struct {
				TurnID string `json:"turn_id"`
				Result struct {
					ResultIndex *int `json:"result_index"`
				} `json:"result"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return fmt.Errorf("decode event %d: %w", ev.Sequence, err)
			}
			if turn == "-" {
				turn = "1"
			}
			if ev.Type == "control.prompt.dispatched" {
				ordinal++
				turn = fmt.Sprint(ordinal)
			}
			if p.Result.ResultIndex != nil {
				ordinal = *p.Result.ResultIndex + 1
				turn = fmt.Sprint(ordinal)
			}
			if p.TurnID != "" {
				turn = p.TurnID
			}
			if ev.Type == "lifecycle.turn.started" {
				var native struct {
					Params struct {
						Turn struct {
							ID string `json:"id"`
						} `json:"turn"`
					} `json:"params"`
				}
				if err := json.Unmarshal(ev.Raw, &native); err == nil && native.Params.Turn.ID != "" {
					turn = native.Params.Turn.ID
				}
			}
		}
		if !page.HasMore || len(page.Events) == 0 {
			break
		}
	}
	seq := s.LastSequence
	lastEvent := "-"
	if s.LastEventAt != nil {
		lastEvent = s.LastEventAt.Format("2006-01-02T15:04:05Z07:00")
	}
	value := map[string]any{"session_id": s.SessionID, "state": s.State, "generation": s.Generation, "continuable": s.Continuable, "continuable_reason": s.ContinuableReason, "runtime": s.Runtime, "provider_session_id": s.ProviderSessionID, "container_lease": s.ContainerLease, "turn": turn, "pending_approvals": len(a.Approvals), "last_event_seq": seq}
	value["last_event_at"], value["idle_seconds"], value["active_subagents"], value["running_turn"] = s.LastEventAt, s.IdleSeconds, s.ActiveSubagents, s.RunningTurn
	value["turn_started_at"] = s.TurnStartedAt
	value["pending_approvals"], value["approvals_available"] = approvalCount, approvalsAvailable
	return WriteOutput(ctx.Out, ctx.Config, value, fmt.Sprintf("%s %s turn=%s approvals=%s seq=%d last_event_at=%s idle=%.1fs active_subagents=%d", s.SessionID, s.State, turn, approvalText, seq, lastEvent, s.IdleSeconds, s.ActiveSubagents))
}

func terminalSessionState(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "terminated", "timed_out", "crashed", "lost", "indeterminate":
		return true
	default:
		return false
	}
}
