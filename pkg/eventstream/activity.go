package eventstream

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// Activity is reconstructed from immutable events, including after daemon
// restart. Control traffic and stderr never refresh the provider clock.
type Activity struct {
	LastEventAt     *time.Time `json:"last_event_at"`
	IdleSeconds     float64    `json:"idle_seconds"`
	ActiveSubagents int        `json:"active_subagents"`
	RunningTurn     bool       `json:"running_turn"`
	TurnStartedAt   *time.Time `json:"turn_started_at"`
}

func SessionActivity(ctx context.Context, store durable.Store, session durable.Session, now time.Time) (Activity, error) {
	activity := Activity{RunningTurn: session.ActiveGeneration > 0 && !session.State.Terminal()}
	if activity.RunningTurn {
		at := session.CreatedAt
		activity.TurnStartedAt = &at
	}
	active := map[string]bool{}
	aliases := map[string]string{}
	baseline := session.CreatedAt
	var cursor int64
	for {
		page, err := store.ListEvents(ctx, durable.EventQuery{SessionID: session.ID, AfterSequence: cursor, Limit: 1000})
		if err != nil {
			return activity, err
		}
		for _, ev := range page.Events {
			cursor = ev.Sequence
			if ev.Stream == durable.StreamProviderStdout {
				at := ev.Timestamp
				activity.LastEventAt = &at
				baseline = at
			}
			switch ev.Type {
			case "lifecycle.turn.started", "control.prompt.dispatched", "control.steer.dispatched":
				activity.RunningTurn = true
				at := ev.Timestamp
				activity.TurnStartedAt = &at
			case "turn.completed", "turn.failed":
				activity.RunningTurn = false
			}
			if ev.Generation == session.ActiveGeneration && strings.HasPrefix(ev.Type, "subagent.") {
				var p struct {
					ID     string `json:"subagent_thread_id"`
					ToolID string `json:"task_tool_use_id"`
				}
				_ = json.Unmarshal(ev.Payload, &p)
				if p.ID != "" {
					if p.ToolID != "" {
						aliases[p.ToolID] = p.ID
						delete(active, p.ToolID)
					}
					if canonical := aliases[p.ID]; canonical != "" {
						p.ID = canonical
					}
					switch ev.Type {
					case "subagent.completed", "subagent.failed", "subagent.turn.completed", "subagent.turn.failed":
						delete(active, p.ID)
						delete(active, p.ToolID)
					case "subagent.started", "subagent.lifecycle.turn.started":
						active[p.ID] = true
					}
				}
			}
		}
		if !page.HasMore || len(page.Events) == 0 {
			break
		}
	}
	activity.IdleSeconds = max(0, now.Sub(baseline).Seconds())
	if session.State.Terminal() {
		activity.RunningTurn = false
		clear(active)
	}
	activity.ActiveSubagents = len(active)
	return activity, nil
}
