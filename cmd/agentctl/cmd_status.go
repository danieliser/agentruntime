package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	if !terminalSessionState(s.State) {
		a, e = approvals(ctx, args[0])
		if e != nil {
			return e
		}
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
	value := map[string]any{"session_id": s.SessionID, "state": s.State, "turn": turn, "pending_approvals": len(a.Approvals), "last_event_seq": seq}
	return WriteOutput(ctx.Out, ctx.Config, value, fmt.Sprintf("%s %s turn=%s approvals=%d seq=%d", s.SessionID, s.State, turn, len(a.Approvals), seq))
}

func terminalSessionState(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "terminated", "timed_out", "crashed", "lost", "indeterminate":
		return true
	default:
		return false
	}
}
