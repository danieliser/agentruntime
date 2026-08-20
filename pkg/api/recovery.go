package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// RestoreRecoveredSessions reconnects runtime handles discovered before the
// HTTP server starts. Only generations with durable identity proof can be
// reconstructed; unversioned runtime processes are stopped.
func (s *Server) RestoreRecoveredSessions(recovered []*session.Session, confirmedRuntimes ...string) {
	discovered := make(map[string]struct{}, len(recovered))
	groups := make(map[string][]*session.Session)
	for _, sess := range recovered {
		if sess == nil || sess.Handle == nil {
			continue
		}
		if info := sess.Handle.RecoveryInfo(); info != nil && info.SessionID != "" && info.Generation > 0 {
			key := recoveryGenerationKey(info.SessionID, info.Generation)
			groups[key] = append(groups[key], sess)
		}
	}
	duplicates := make(map[string]struct{})
	for key, sessions := range groups {
		if len(sessions) < 2 {
			continue
		}
		duplicates[key] = struct{}{}
		discovered[key] = struct{}{}
		for _, sess := range sessions {
			_ = sess.Handle.Kill()
		}
		s.recordRecoveredIndeterminate(sessions[0].ID, sessions[0].Handle.RecoveryInfo().Generation, "duplicate runtime containers claim the same logical generation")
	}
	for _, sess := range recovered {
		if sess == nil || sess.Handle == nil {
			continue
		}
		info := sess.Handle.RecoveryInfo()
		if info != nil && info.Generation > 0 {
			key := recoveryGenerationKey(info.SessionID, info.Generation)
			if _, duplicate := duplicates[key]; duplicate {
				continue
			}
		}
		if info == nil || info.Generation < 1 || s.durableStore == nil || s.eventBroker == nil {
			_ = sess.Handle.Kill()
			log.Printf("[session %s] stopped recovered process without durable generation proof", sess.ID)
			continue
		}
		if err := s.adoptRecoveredStartingGeneration(sess, *info); err != nil {
			log.Printf("[session %s] durable generation adoption failed: %v", sess.ID, err)
			_ = sess.Handle.Kill()
			s.settleRecoveredStartingIndeterminate(sess, *info, err)
			continue
		}
		if err := s.restoreDurableNativeSession(sess, *info); err != nil {
			log.Printf("[session %s] durable recovery failed: %v", sess.ID, err)
			_ = sess.Handle.Kill()
			s.finalizeV1Session(sess.ID, runtime.ExitResult{Code: -1, Err: err}, err)
			continue
		}
		discovered[recoveryGenerationKey(info.SessionID, info.Generation)] = struct{}{}
	}
	s.reconcileGenerationlessAdmissions(confirmedRuntimes)
	s.reconcileMissingGenerations(discovered, confirmedRuntimes)
	s.reconcileTerminalRetention(confirmedRuntimes)
}

func (s *Server) reconcileGenerationlessAdmissions(confirmedRuntimes []string) {
	if s.durableStore == nil || s.eventBroker == nil || len(confirmedRuntimes) == 0 {
		return
	}
	confirmed := make(map[string]struct{}, len(confirmedRuntimes))
	for _, name := range confirmedRuntimes {
		confirmed[name] = struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := s.durableStore.ListSessions(ctx)
	if err != nil {
		log.Printf("generationless admission reconciliation could not list sessions: %v", err)
		return
	}
	for _, stored := range sessions {
		if stored.State.Terminal() || stored.ActiveGeneration != 0 || (stored.State != durable.StateCreated && stored.State != durable.StateStarting) {
			continue
		}
		if _, ok := confirmed[stored.Runtime]; !ok {
			continue
		}
		cause := durable.NewError(durable.CodeIndeterminate, "reconcile_generationless_admission", "admitted session has no runtime generation after confirmed recovery", nil)
		if _, err := s.settleAdmittedSession(ctx, stored.ID, durable.StateIndeterminate, "indeterminate", cause); err != nil {
			log.Printf("[session %s] generationless admission settlement failed: %v", stored.ID, err)
			continue
		}
		log.Printf("[session %s] generationless admission terminalized as indeterminate", stored.ID)
	}
}

func (s *Server) reconcileTerminalRetention(confirmedRuntimes []string) {
	if s.durableStore == nil {
		return
	}
	confirmed := make(map[string]struct{}, len(confirmedRuntimes))
	for _, name := range confirmedRuntimes {
		confirmed[name] = struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := s.durableStore.ListSessions(ctx)
	if err != nil {
		log.Printf("terminal retention reconciliation could not list sessions: %v", err)
		return
	}
	for _, stored := range sessions {
		if !stored.State.Terminal() {
			continue
		}
		if _, ok := confirmed[stored.Runtime]; !ok {
			continue
		}
		s.releaseEphemeralSession(stored)
	}
}

func (s *Server) settleRecoveredStartingIndeterminate(sess *session.Session, info runtime.RecoveryInfo, cause error) {
	const op = "settle_recovered_starting_indeterminate"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := s.durableStore.GetSession(ctx, sess.ID)
	if err != nil || stored.State.Terminal() {
		if err != nil {
			log.Printf("[session %s] indeterminate settlement lookup failed: %v", sess.ID, err)
		}
		return
	}
	generationNumber := stored.ActiveGeneration
	createGeneration := generationNumber == 0
	if generationNumber > 0 && info.Generation == generationNumber+1 {
		current, generationErr := s.durableStore.GetGeneration(ctx, stored.ID, generationNumber)
		if generationErr == nil && current.State.Terminal() {
			createGeneration = true
		}
	}
	if createGeneration {
		runtimeID := runtimeGenerationIdentity(sess.Handle, sess.RuntimeName, sess.ID, info.Generation)
		if runtimeID == "" {
			log.Printf("[session %s] cannot settle unverified runtime without stable identity", sess.ID)
			return
		}
		generation, createErr := s.durableStore.CreateGeneration(ctx, durable.CreateGenerationParams{
			SessionID: stored.ID, Runtime: stored.Runtime, ContainerID: runtimeID,
			ImageReference: info.ImageReference, ImageDigest: info.ImageDigest,
			SandboxProfile: info.SandboxProfile, DockerLogDriver: generationDockerLogDriver(stored.Runtime, true),
			CreatedAt: time.Now().UTC(),
		})
		if createErr != nil {
			log.Printf("[session %s] %s failed to record unverified generation: %v", sess.ID, op, createErr)
			return
		}
		generationNumber = generation.Number
	}
	s.recordRecoveredIndeterminate(sess.ID, generationNumber, cause.Error())
}

// adoptRecoveredStartingGeneration closes the crash window between Docker
// accepting a labeled container and AgentD committing that generation. Only
// an exact match to the already-durable admission identity is reconstructable.
// Each transition is repeat-safe so another daemon crash can resume adoption.
func (s *Server) adoptRecoveredStartingGeneration(sess *session.Session, info runtime.RecoveryInfo) error {
	const op = "adopt_recovered_starting_generation"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := s.durableStore.GetSession(ctx, sess.ID)
	if err != nil {
		return err
	}
	if stored.State.Terminal() {
		return nil
	}
	transitionSession := false
	var generation durable.Generation
	createGeneration := false
	switch stored.State {
	case durable.StateStarting:
		transitionSession = true
		switch stored.ActiveGeneration {
		case 0:
			createGeneration = info.Generation == 1
		case info.Generation:
			generation, err = s.durableStore.GetGeneration(ctx, stored.ID, info.Generation)
		default:
			return durable.NewError(durable.CodeIndeterminate, op, "starting session points at an unexpected generation", nil)
		}
	case durable.StateRunning:
		switch {
		case stored.ActiveGeneration == info.Generation:
			generation, err = s.durableStore.GetGeneration(ctx, stored.ID, info.Generation)
			if err == nil && generation.State == durable.GenerationRunning {
				return nil
			}
		case stored.ActiveGeneration+1 == info.Generation:
			previous, previousErr := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
			if previousErr != nil {
				return previousErr
			}
			if previous.State != durable.GenerationLost {
				return durable.NewError(durable.CodeIndeterminate, op, "replacement container has no lost predecessor", nil)
			}
			createGeneration = true
		default:
			return nil
		}
	default:
		return nil
	}
	if err != nil {
		return err
	}
	if !createGeneration && generation.Number == 0 {
		return durable.NewError(durable.CodeIndeterminate, op, "recovered generation was not durably identifiable", nil)
	}
	var admittedRequest SessionRequest
	if err := json.Unmarshal(stored.RequestManifest, &admittedRequest); err != nil {
		return durable.NewError(durable.CodeIndeterminate, op, "decode admitted sandbox profile", err)
	}
	expectedSandboxProfile := requestSandboxProfile("docker", true, admittedRequest)
	if stored.Runtime != "docker" || sess.RuntimeName != "docker" ||
		info.IdempotencyKey != stored.IdempotencyKey || info.RequestHash != stored.RequestHash ||
		info.AgentName != stored.Agent || info.ImageReference == "" || info.ImageReference == "unknown" ||
		!strings.HasPrefix(info.ImageDigest, "sha256:") || info.SandboxProfile != expectedSandboxProfile {
		return durable.NewError(durable.CodeIndeterminate, op, "container labels do not prove the admitted starting generation", nil)
	}
	runtimeID := runtimeGenerationIdentity(sess.Handle, sess.RuntimeName, sess.ID, info.Generation)
	if runtimeID == "" {
		return durable.NewError(durable.CodeIndeterminate, op, "recovered container has no stable runtime identity", nil)
	}

	if createGeneration {
		generation, err = s.durableStore.CreateGeneration(ctx, durable.CreateGenerationParams{
			SessionID: stored.ID, Runtime: stored.Runtime, ContainerID: runtimeID,
			ImageReference: info.ImageReference, ImageDigest: info.ImageDigest,
			SandboxProfile: info.SandboxProfile, DockerLogDriver: generationDockerLogDriver("docker", true),
			CreatedAt: time.Now().UTC(),
		})
	}
	if err != nil {
		return err
	}
	if generation.Number != info.Generation || generation.Runtime != stored.Runtime || generation.ContainerID != runtimeID ||
		generation.ImageReference != info.ImageReference || generation.ImageDigest != info.ImageDigest ||
		generation.SandboxProfile != info.SandboxProfile {
		return durable.NewError(durable.CodeIndeterminate, op, "recovered container does not match the partially committed generation", nil)
	}
	if generation.State == durable.GenerationStarting {
		generation, err = s.durableStore.TransitionGeneration(ctx, durable.TransitionGenerationParams{
			SessionID: stored.ID, Generation: generation.Number, From: durable.GenerationStarting,
			To: durable.GenerationRunning, At: time.Now().UTC(),
		})
		if err != nil {
			return err
		}
	}
	if generation.State != durable.GenerationRunning {
		return durable.NewError(durable.CodeIndeterminate, op, "partially committed generation is not recoverable", nil)
	}
	if transitionSession {
		_, err = s.durableStore.TransitionSession(ctx, durable.TransitionSessionParams{
			SessionID: stored.ID, From: durable.StateStarting, To: durable.StateRunning, At: time.Now().UTC(),
		})
	}
	return err
}

func (s *Server) recordRecoveredIndeterminate(sessionID string, generation int64, reason string) {
	err := durable.NewError(durable.CodeIndeterminate, "reconcile_duplicate_generation", reason, nil)
	if s.eventBroker != nil {
		_, appendErr := s.eventBroker.IngestTerminal(context.Background(), eventstream.TerminalParams{
			SessionID: sessionID, Generation: generation, Timestamp: time.Now().UTC(),
			Reason: "indeterminate", ExitCode: -1, Error: reason,
		})
		err = errors.Join(err, appendErr)
	}
	s.finalizeV1Session(sessionID, runtime.ExitResult{Code: -1, Err: err}, err)
}

func (s *Server) reconcileMissingGenerations(discovered map[string]struct{}, confirmedRuntimes []string) {
	if s.durableStore == nil || len(confirmedRuntimes) == 0 {
		return
	}
	confirmed := make(map[string]struct{}, len(confirmedRuntimes))
	for _, name := range confirmedRuntimes {
		confirmed[name] = struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := s.durableStore.ListSessions(ctx)
	if err != nil {
		log.Printf("durable startup reconciliation failed to list sessions: %v", err)
		return
	}
	for _, stored := range sessions {
		if stored.State.Terminal() || stored.ActiveGeneration < 1 {
			continue
		}
		if _, ok := confirmed[stored.Runtime]; !ok {
			continue
		}
		if _, ok := discovered[recoveryGenerationKey(stored.ID, stored.ActiveGeneration)]; ok {
			continue
		}
		generation, err := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
		if err != nil {
			log.Printf("[session %s] startup reconciliation generation lookup failed: %v", stored.ID, err)
			continue
		}
		if generation.State.Terminal() {
			continue
		}
		if _, err := s.durableStore.TransitionGeneration(ctx, durable.TransitionGenerationParams{
			SessionID: stored.ID, Generation: generation.Number, From: generation.State,
			To: durable.GenerationLost, At: time.Now().UTC(),
		}); err != nil {
			log.Printf("[session %s] mark missing generation %d lost: %v", stored.ID, generation.Number, err)
			continue
		}
		log.Printf("[session %s] generation %d is missing after confirmed %s recovery; marked lost and eligible for explicit resume", stored.ID, generation.Number, stored.Runtime)
	}
}

func recoveryGenerationKey(sessionID string, generation int64) string {
	return fmt.Sprintf("%s\x00%d", sessionID, generation)
}

func (s *Server) restoreDurableNativeSession(sess *session.Session, info runtime.RecoveryInfo) error {
	const op = "restore_durable_native_session"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := s.durableStore.GetSession(ctx, sess.ID)
	if err != nil {
		return err
	}
	if stored.State.Terminal() {
		return durable.NewError(durable.CodeInvalidState, op, "terminal session still has a running container", nil)
	}
	if stored.State != durable.StateRunning || stored.ActiveGeneration != info.Generation {
		return durable.NewError(durable.CodeIndeterminate, op, "runtime labels do not match the active running generation", nil)
	}
	generation, err := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
	if err != nil {
		return err
	}
	if generation.State != durable.GenerationRunning || generation.ContainerID != runtimeGenerationIdentity(sess.Handle, sess.RuntimeName, sess.ID, generation.Number) {
		return durable.NewError(durable.CodeIndeterminate, op, "container identity does not match the durable generation", nil)
	}
	provider := nativeprotocol.Provider(stored.Agent)
	if provider != nativeprotocol.ProviderClaude && provider != nativeprotocol.ProviderCodex {
		return durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("agent %q has no native recovery transport", stored.Agent), nil)
	}
	var manifest SessionRequest
	if len(stored.RequestManifest) > 0 {
		if err := json.Unmarshal(stored.RequestManifest, &manifest); err != nil {
			return durable.NewError(durable.CodeIndeterminate, op, "decode stored request manifest", err)
		}
	}
	if _, err := resolveExecutionPolicy(&manifest, stored.Runtime); err != nil {
		return durable.NewError(durable.CodeIndeterminate, op, "stored execution policy is no longer enforceable", err)
	}
	if _, err := resolveStructuredOutput(&manifest); err != nil {
		return durable.NewError(durable.CodeIndeterminate, op, "stored structured-output contract is no longer enforceable", err)
	}
	sess.AgentName = stored.Agent
	sess.SetRunning(sess.Handle)
	rt := s.RuntimeFor(stored.Runtime)
	spawnConfig := runtime.SpawnConfig{
		SessionID: stored.ID, Generation: generation.Number, ExecutionPolicyHash: manifestPolicyHash(stored.RequestManifest),
		AgentName: stored.Agent, Request: &manifest,
	}
	var active activeNativeSessionRef
	if err := AttachNativeSessionIO(
		sess, s.logDir, provider, generation.Number, generation.ProviderID,
		"", diagnosticRedactions(manifest), !manifest.Interactive,
		true, nativePolicy(manifest), manifest.StructuredOutput, s.eventBroker,
		func() string {
			current := active.Load()
			if current == nil {
				return ""
			}
			return current.terminalReason()
		},
		func(transport nativeprotocol.Transport) {
			current := s.setNativeTransport(sess.ID, transport)
			active.Store(current)
			s.armNativeTimeout(sess.ID, generation.Number, current, manifest.EffectiveTimeout(), generation.CreatedAt)
		},
		func(result runtime.ExitResult, streamErr error) {
			current := active.Load()
			s.clearNativeTransport(sess.ID, current)
			var override durable.SessionState
			var reason string
			if current != nil {
				override = current.terminalState()
				reason = current.terminalReceiptReason()
			}
			s.finalizeV1SessionClassified(sess.ID, result, override, reason, streamErr)
		},
		classifyNativeExitFailure(rt, spawnConfig),
	); err != nil {
		return classifyNativeBootstrapFailure(rt, spawnConfig, err)
	}
	log.Printf("[session %s] recovered native generation %d at durable sequence %d", sess.ID, generation.Number, stored.LastSequence)
	return nil
}
