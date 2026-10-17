package main

import (
	"context"
	"fmt"
	"io"
	"time"
)

func checkStalled(ctx context.Context, api *APIClient, id string, stall time.Duration, cfg Config, out io.Writer) error {
	session, err := api.GetSession(ctx, id)
	if err != nil {
		return err
	}
	idle := session.IdleSeconds
	// A new prompt after a long warm idle gets its own silence window. GET's
	// idle_seconds remains strictly the time since provider output.
	if session.TurnStartedAt != nil {
		idle = min(idle, time.Since(*session.TurnStartedAt).Seconds())
	}
	if session.State != "running" || !session.RunningTurn || idle < stall.Seconds() {
		return nil
	}
	notice := struct {
		Reason          string  `json:"reason"`
		IdleSeconds     float64 `json:"idle_seconds"`
		ActiveSubagents int     `json:"active_subagents"`
	}{"stalled", idle, session.ActiveSubagents}
	if err := WriteOutput(out, cfg, notice, fmt.Sprintf("stalled: provider silent for %.1fs active_subagents=%d", idle, session.ActiveSubagents)); err != nil {
		return err
	}
	return WaitReasonError("stalled")
}
