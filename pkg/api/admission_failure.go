package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

const unstartedRuntimeIdentityPrefix = "agentd-unstarted:"

func classifyRuntimeFailure(cause error) error {
	var egressErr *runtime.EgressError
	if !errors.As(cause, &egressErr) {
		return cause
	}
	code := durable.CodeEgressPreflightFailed
	if egressErr.Code == runtime.EgressDenied {
		code = durable.CodeEgressDenied
	}
	return durable.NewError(code, "restricted_egress", egressErr.Error(), cause)
}

func classifyNativeBootstrapFailure(rt runtime.Runtime, cfg runtime.SpawnConfig, cause error) error {
	if egressFailure := inspectNativeEgressFailure(rt, cfg); egressFailure != nil {
		return egressFailure
	}
	message := "provider failed before completing bounded native startup"
	if errors.Is(cause, context.DeadlineExceeded) {
		message = "provider did not complete bounded native startup"
	}
	return durable.NewError(durable.CodeProviderStartupFailed, "bootstrap_native_provider", message, cause)
}

func classifyNativeExitFailure(rt runtime.Runtime, cfg runtime.SpawnConfig) func(runtime.ExitResult) error {
	return func(runtime.ExitResult) error {
		return inspectNativeEgressFailure(rt, cfg)
	}
}

func inspectNativeEgressFailure(rt runtime.Runtime, cfg runtime.SpawnConfig) error {
	inspector, ok := rt.(runtime.EgressFailureInspector)
	if !ok || cfg.Request == nil || cfg.Request.ExecutionPolicy == nil || !cfg.Request.ExecutionPolicy.EgressDiagnostics {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	egressCause := inspector.InspectEgressFailure(ctx, cfg)
	var typedEgress *runtime.EgressError
	if !errors.As(egressCause, &typedEgress) {
		return nil
	}
	return classifyRuntimeFailure(typedEgress)
}

func (s *Server) checkRuntimeAdmission(ctx context.Context, request SessionRequest, rt runtime.Runtime) error {
	if request.IdempotencyKey == "" {
		return nil
	}
	if s.durableStore != nil {
		if _, err := s.durableStore.GetSessionByIdempotencyKey(ctx, request.IdempotencyKey); err == nil {
			// Existing admissions must remain inspectable/idempotent even when the
			// runtime later becomes unavailable. admitV1Session still validates
			// the canonical request hash before returning the existing identity.
			return nil
		} else if !durable.IsCode(err, durable.CodeNotFound) {
			return err
		}
	}
	if s.readiness != nil {
		if handled, err := s.readiness.admission(rt.Name(), time.Now().UTC()); handled {
			if err != nil {
				return durable.NewError(durable.CodeRuntimeUnavailable, "check_runtime_admission", "execution runtime is unavailable", err)
			}
			return nil
		}
	}
	checker, ok := rt.(runtime.AdmissionChecker)
	if !ok {
		return nil
	}
	if err := checker.CheckAdmission(ctx); err != nil {
		return durable.NewError(durable.CodeRuntimeUnavailable, "check_runtime_admission", "execution runtime is unavailable", err)
	}
	return nil
}

func (s *Server) writeAdmittedFailure(c *gin.Context, sessionID string, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settled, err := s.settleAdmittedSession(ctx, sessionID, durable.StateFailed, "failed", cause)
	if err == nil {
		s.writeV1Session(c, http.StatusCreated, settled)
		return
	}
	log.Printf("[session %s] admitted runtime failure could not be terminalized: %v", sessionID, err)
	envelope := gin.H{"api_version": "v1", "error": apiErrorEnvelope{Code: durable.CodeIndeterminate, Message: "admitted session settlement failed"}}
	if stored, lookupErr := s.durableStore.GetSession(ctx, sessionID); lookupErr == nil {
		envelope["data"] = s.v1SessionView(c, stored)
	}
	c.JSON(http.StatusServiceUnavailable, envelope)
}

// settleAdmittedSession closes an admission that cannot reach a live runtime.
// A synthetic generation records the failed launch attempt without pretending
// that a process/container existed, allowing the ordinary immutable terminal
// event and receipt contracts to remain intact.
func (s *Server) settleAdmittedSession(ctx context.Context, sessionID string, state durable.SessionState, reason string, cause error) (durable.Session, error) {
	const op = "settle_admitted_session"
	if state != durable.StateFailed && state != durable.StateCancelled && state != durable.StateIndeterminate {
		return durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "unsupported admission terminal state", nil)
	}
	if normalized, valid := durable.NormalizeTerminalReason(state, reason); !valid {
		return durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "invalid admission terminal reason", nil)
	} else {
		reason = normalized
	}
	stored, generation, err := s.ensureAdmissionGeneration(ctx, sessionID)
	if err != nil {
		return durable.Session{}, err
	}
	if stored.State.Terminal() {
		return stored, nil
	}
	timestamp := time.Now().UTC()
	if timestamp.Before(stored.UpdatedAt) {
		timestamp = stored.UpdatedAt
	}
	if timestamp.Before(generation.UpdatedAt) {
		timestamp = generation.UpdatedAt
	}
	errorText := ""
	if cause != nil {
		errorText = cause.Error()
	}
	if _, err := s.eventBroker.IngestTerminal(ctx, eventstream.TerminalParams{
		SessionID: stored.ID, Generation: generation.Number, Timestamp: timestamp,
		Reason: reason, ExitCode: -1, Error: errorText,
	}); err != nil {
		return durable.Session{}, durable.NewError(durable.CodeIndeterminate, op, "commit admission terminal evidence", err)
	}
	stored, err = s.durableStore.GetSession(ctx, stored.ID)
	if err != nil {
		return durable.Session{}, err
	}
	generationTo := durable.GenerationExited
	if state == durable.StateIndeterminate {
		generationTo = durable.GenerationIndeterminate
	}
	exitCode := -1
	result, err := s.durableStore.FinalizeSession(ctx, durable.FinalizeSessionParams{
		From: stored.State, GenerationFrom: generation.State, GenerationTo: generationTo,
		Receipt: durable.TerminalReceipt{
			SessionID: stored.ID, Generation: generation.Number, State: state, Reason: reason,
			ExitCode: &exitCode, StartedAt: generation.CreatedAt, EndedAt: timestamp,
			OutputHash: outputHash(session.LogFilePath(s.logDir, stored.ID)), LastSequence: stored.LastSequence,
		},
	})
	if err != nil {
		return durable.Session{}, err
	}
	s.releaseEphemeralSession(result.Session)
	return result.Session, nil
}

func (s *Server) ensureAdmissionGeneration(ctx context.Context, sessionID string) (durable.Session, durable.Generation, error) {
	const op = "ensure_admission_generation"
	stored, err := s.durableStore.GetSession(ctx, sessionID)
	if err != nil {
		return durable.Session{}, durable.Generation{}, err
	}
	if stored.State.Terminal() {
		return stored, durable.Generation{}, nil
	}
	if stored.State == durable.StateCreated {
		stored, err = s.durableStore.TransitionSession(ctx, durable.TransitionSessionParams{
			SessionID: stored.ID, From: durable.StateCreated, To: durable.StateStarting, At: time.Now().UTC(),
		})
		if err != nil {
			return durable.Session{}, durable.Generation{}, err
		}
	}
	if stored.ActiveGeneration > 0 {
		generation, err := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
		return stored, generation, err
	}
	if stored.State != durable.StateStarting {
		return durable.Session{}, durable.Generation{}, durable.NewError(durable.CodeInvalidState, op, "generationless settlement requires a created or starting session", nil)
	}
	var request SessionRequest
	if err := json.Unmarshal(stored.RequestManifest, &request); err != nil {
		return durable.Session{}, durable.Generation{}, durable.NewError(durable.CodeIndeterminate, op, "decode admitted request manifest", err)
	}
	generationNumber := stored.ActiveGeneration + 1
	generation, err := s.durableStore.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: stored.ID, Runtime: stored.Runtime,
		ContainerID:     fmt.Sprintf("%s%s:%d", unstartedRuntimeIdentityPrefix, stored.ID, generationNumber),
		ImageReference:  resolvedImageReference(request, stored.Runtime),
		SandboxProfile:  requestSandboxProfile(stored.Runtime, nativeV1Agent(stored.Agent), request),
		DockerLogDriver: generationDockerLogDriver(stored.Runtime, nativeV1Agent(stored.Agent)),
		CreatedAt:       time.Now().UTC(),
	})
	if err != nil {
		latest, lookupErr := s.durableStore.GetSession(ctx, stored.ID)
		if lookupErr == nil && latest.ActiveGeneration > 0 {
			generation, lookupErr = s.durableStore.GetGeneration(ctx, latest.ID, latest.ActiveGeneration)
			return latest, generation, lookupErr
		}
		return durable.Session{}, durable.Generation{}, err
	}
	stored, err = s.durableStore.GetSession(ctx, stored.ID)
	return stored, generation, err
}
