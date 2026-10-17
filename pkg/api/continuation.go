package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
)

type continuationStatus struct {
	Continuable bool
	Reason      string
}

func (s *Server) sessionContinuation(ctx context.Context, stored durable.Session) continuationStatus {
	deny := func(reason string) continuationStatus { return continuationStatus{Reason: reason} }
	if stored.State.Terminal() && stored.State != durable.StateCompleted {
		return deny(fmt.Sprintf("session is %s and cannot continue", stored.State))
	}
	if active := s.nativeSession(stored.ID); active != nil {
		active.mu.Lock()
		idle := active.idle && !active.terminalClaimed
		active.mu.Unlock()
		if idle && (active.lease == nil || active.lease.IsIdle()) {
			return continuationStatus{true, "idle provider transport accepts a prompt"}
		}
		return deny("provider turn is still active; use steer instead of prompt")
	}
	if stored.State != durable.StateCompleted && stored.State != durable.StateRunning {
		return deny("generation admission is pending or unresolved; no prompt can be accepted")
	}
	if !nativeV1Agent(stored.Agent) || s.agents.Get(stored.Agent) == nil || s.RuntimeFor(stored.Runtime) == nil {
		return deny("stored native provider or runtime is unavailable")
	}
	if len(stored.SecretGrants) != 0 {
		return deny("stored secret grants require explicit regrant through a new request")
	}
	generation, err := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
	if err != nil || generation.ProviderID == "" {
		return deny("completed generation has no recoverable provider identity")
	}
	if stored.State != durable.StateCompleted || generation.State != durable.GenerationExited {
		if generation.State != durable.GenerationLost || !s.generationEndedNormally(ctx, stored, generation.Number) {
			return deny("provider generation is active or unresolved; use steer for an active turn or explicit resume after loss")
		}
	}
	var request SessionRequest
	if err := json.Unmarshal(stored.RequestManifest, &request); err != nil {
		return deny("stored request manifest is unavailable")
	}
	if request.ExecutionPolicy != nil && request.ExecutionPolicy.Workspace == "ephemeral" && request.ExecutionPolicy.WorkspaceRetention == "terminal_receipt" {
		return deny("ephemeral provider state was released at terminal receipt")
	}
	if stored.Runtime == "docker" {
		if _, retained, err := s.durableProviderStateRoot(ctx, stored); err != nil || !retained {
			return deny("Docker provider state was not retained")
		}
	}
	return continuationStatus{true, "normally ended generation can resume native provider context"}
}

// probeSessionControl makes input retry identity stable across generation
// boundaries. Requested-without-dispatched remains indeterminate after restart.
func (s *Server) probeSessionControl(ctx context.Context, params eventstream.ControlParams) (bool, error) {
	for generation := params.Generation; generation > 0; generation-- {
		params.Generation = generation
		probe, err := s.eventBroker.ProbeControl(ctx, params)
		if err != nil {
			return false, err
		}
		if probe.AlreadyDispatched {
			return true, nil
		}
	}
	return false, nil
}

// A lost transport may have been idle at daemon shutdown. Only durable turn
// completion without a later turn/input intent proves that prompt can wake it.
func (s *Server) generationEndedNormally(ctx context.Context, stored durable.Session, generation int64) bool {
	idle := false
	after := int64(0)
	for {
		page, err := s.durableStore.ListEvents(ctx, durable.EventQuery{SessionID: stored.ID, AfterSequence: after, Limit: 1000})
		if err != nil {
			return false
		}
		for _, event := range page.Events {
			after = event.Sequence
			if event.Generation != generation {
				continue
			}
			switch event.Type {
			case "turn.completed":
				idle = true
			case "turn.started", "turn.failed", "turn.superseded":
				idle = false
			default:
				if event.Stream == durable.StreamControl && strings.HasSuffix(event.Type, ".requested") {
					idle = false
				}
			}
		}
		if !page.HasMore {
			return idle
		}
	}
}
