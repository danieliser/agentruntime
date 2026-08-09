package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// RestoreRecoveredSessions reconnects runtime handles discovered before the
// HTTP server starts. Durable generations re-enter the native ledger through
// retained source positions; generation-zero sessions retain legacy recovery.
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
			s.restoreLegacySession(sess)
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
	s.reconcileMissingGenerations(discovered, confirmedRuntimes)
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
	if provider == nativeprotocol.ProviderCodex && generation.ProviderID == "" {
		return durable.NewError(durable.CodeIndeterminate, op, "Codex thread identity was not durably observed before restart", nil)
	}
	var manifest struct {
		Interactive bool `json:"interactive"`
	}
	if len(stored.RequestManifest) > 0 {
		if err := json.Unmarshal(stored.RequestManifest, &manifest); err != nil {
			return durable.NewError(durable.CodeIndeterminate, op, "decode stored request manifest", err)
		}
	}
	sess.AgentName = stored.Agent
	sess.SetRunning(sess.Handle)
	var active *activeNativeSession
	if err := AttachNativeSessionIO(
		sess, s.logDir, provider, generation.Number, generation.ProviderID,
		"", !manifest.Interactive,
		true, s.eventBroker,
		func() string {
			if active == nil {
				return ""
			}
			return active.terminalReason()
		},
		func(transport nativeprotocol.Transport) {
			active = s.setNativeTransport(sess.ID, transport)
		},
		func(result runtime.ExitResult, streamErr error) {
			s.clearNativeTransport(sess.ID, active)
			var override durable.SessionState
			if active != nil {
				override = active.terminalState()
			}
			s.finalizeV1SessionAs(sess.ID, result, override, streamErr)
		},
	); err != nil {
		return err
	}
	log.Printf("[session %s] recovered native generation %d at durable sequence %d", sess.ID, generation.Number, stored.LastSequence)
	return nil
}

func (s *Server) restoreLegacySession(sess *session.Session) {
	var restoredBytes int64
	logPath, exists, err := session.ExistingLogFilePath(s.logDir, sess.ID)
	if err != nil {
		log.Printf("[session %s] warning: check replay log failed: %v", sess.ID, err)
	} else if exists {
		if err := sess.Replay.LoadFromFile(logPath); err != nil {
			log.Printf("[session %s] warning: restore replay from %s failed: %v", sess.ID, logPath, err)
		} else {
			restoredBytes = sess.Replay.TotalBytes()
		}
	}
	AttachSessionIO(sess, s.logDir)
	log.Printf("recovered legacy session %s: replay loaded (%d bytes), stdio reattached", sess.ID, restoredBytes)
}
