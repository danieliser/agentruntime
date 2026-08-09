package api

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// spawnDurableSession is the non-HTTP native session admission path used by
// named chats. It writes the same v1 ledger as POST /api/v1/sessions.
func (s *Server) spawnDurableSession(ctx context.Context, req SessionRequest) (*session.Session, durable.Session, error) {
	const op = "spawn_durable_session"
	if s.durableStore == nil || s.eventBroker == nil {
		return nil, durable.Session{}, durable.NewError(durable.CodeIndeterminate, op, "durable session services unavailable", nil)
	}
	if !nativeV1Agent(req.Agent) {
		return nil, durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "durable internal spawn requires a native Claude or Codex agent", nil)
	}
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = "internal:" + req.SessionID
	}
	rt := s.RuntimeFor(req.Runtime)
	if rt == nil {
		return nil, durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("unknown runtime %q", req.Runtime), nil)
	}
	mounts := req.EffectiveMounts()
	workDir := effectiveWorkDir(req.WorkDir, mounts)
	if workDir != "" {
		if err := session.ValidateWorkDir(workDir); err != nil {
			return nil, durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "invalid working directory", err)
		}
	}
	ag := s.agents.Get(req.Agent)
	if ag == nil {
		return nil, durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("unknown agent %q", req.Agent), nil)
	}
	if err := validateContextMode(&req, rt.Name()); err != nil {
		return nil, durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "invalid context mode", err)
	}
	switch req.Agent {
	case "claude":
		if req.Claude == nil {
			req.Claude = &ClaudeConfig{}
		}
	case "codex":
		if req.Codex == nil {
			req.Codex = &CodexConfig{}
		}
	}
	var original *session.Session
	if req.ResumeSession != "" {
		original = s.sessions.Get(req.ResumeSession)
		if original != nil && original.VolumeName != "" {
			req.PersistSession = true
		}
	}
	providerID, err := s.lookupResumeSessionID(req.Agent, req.ResumeSession, original)
	if err != nil {
		return nil, durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "resolve provider session", err)
	}
	agentConfig := agent.AgentConfig{
		Model: req.Model, WorkDir: workDir, Env: req.Env, Interactive: req.Interactive,
		NativeStream: true, ResumeSessionID: providerID, Effort: req.Effort, Fast: req.Fast,
	}
	if req.Claude != nil {
		agentConfig.MaxTokens = req.Claude.MaxTurns
		agentConfig.AllowedTools = append([]string(nil), req.Claude.AllowedTools...)
	}
	command, err := ag.BuildCmd("", agentConfig)
	if err != nil {
		return nil, durable.Session{}, durable.NewError(durable.CodeInvalidArgument, op, "build native command", err)
	}
	if _, codex := ag.(*agent.CodexAgent); codex {
		command = []string{"codex", "app-server", "--listen", "stdio://"}
	}

	admission, err := s.admitV1Session(ctx, req, rt.Name())
	if err != nil {
		return nil, durable.Session{}, err
	}
	if !admission.Created {
		existing := s.sessions.Get(admission.Session.ID)
		if existing == nil {
			return nil, admission.Session, durable.NewError(durable.CodeIndeterminate, op, "durable session exists without an attached runtime", nil)
		}
		return existing, admission.Session, nil
	}
	stored := admission.Session
	sess := session.NewSessionWithID(stored.ID, req.TaskID, req.Agent, rt.Name(), req.Tags)
	sess.Contamination = agent.KnownContamination(req.Agent, req.Context == "clean")
	if err := s.prepareSessionDir(sess, &req, workDir); err != nil {
		return nil, stored, durable.NewError(durable.CodeIndeterminate, op, "prepare session directory", err)
	}
	if err := s.sessions.Add(sess); err != nil {
		return nil, stored, err
	}

	volumeName := ""
	if req.PersistSession {
		if original != nil && original.VolumeName != "" {
			volumeName = original.VolumeName
		} else {
			volumeName = "agentruntime-vol-" + sess.ID
		}
		sess.VolumeName = volumeName
	}
	lifecycleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err = s.durableStore.TransitionSession(lifecycleCtx, durable.TransitionSessionParams{
		SessionID: stored.ID, From: durable.StateCreated, To: durable.StateStarting, At: time.Now().UTC(),
	})
	if err != nil {
		s.sessions.Remove(sess.ID)
		return nil, stored, err
	}
	generationNumber := stored.ActiveGeneration + 1
	handle, err := rt.Spawn(ctx, runtime.SpawnConfig{
		SessionID: sess.ID, Generation: generationNumber, IdempotencyKey: stored.IdempotencyKey,
		RequestHash: stored.RequestHash, AgentName: req.Agent,
		Cmd: runtimeSpawnCommand(command, rt.Name(), req.Agent), Prompt: req.Prompt, Model: req.Model,
		Env: req.Env, WorkDir: workDir, TaskID: req.TaskID, Request: &req,
		SessionDir: &sess.SessionDir, VolumeName: volumeName, PTY: req.PTY,
		SandboxProfile: runtimeSandboxProfile(rt.Name(), true),
	})
	if err != nil {
		s.sessions.Remove(sess.ID)
		return nil, stored, durable.NewError(durable.CodeIndeterminate, op, "spawn native runtime", err)
	}
	sess.SetRunning(handle)
	runtimeID := runtimeGenerationIdentity(handle, rt.Name(), sess.ID, generationNumber)
	if runtimeID == "" {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		return nil, stored, durable.NewError(durable.CodeIndeterminate, op, "runtime did not expose a reconstructable identity", nil)
	}
	generation, err := s.durableStore.CreateGeneration(lifecycleCtx, durable.CreateGenerationParams{
		SessionID: sess.ID, Runtime: rt.Name(), ContainerID: runtimeID,
		ImageReference: resolvedImageReference(req, rt.Name()), ImageDigest: runtimeGenerationImageDigest(handle),
		SandboxProfile: runtimeSandboxProfile(rt.Name(), true), DockerLogDriver: generationDockerLogDriver(rt.Name(), true),
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		return nil, stored, err
	}
	if _, err := s.durableStore.TransitionGeneration(lifecycleCtx, durable.TransitionGenerationParams{
		SessionID: sess.ID, Generation: generation.Number, From: durable.GenerationStarting,
		To: durable.GenerationRunning, At: time.Now().UTC(),
	}); err != nil {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		return nil, stored, err
	}
	stored, err = s.durableStore.TransitionSession(lifecycleCtx, durable.TransitionSessionParams{
		SessionID: sess.ID, From: durable.StateStarting, To: durable.StateRunning, At: time.Now().UTC(),
	})
	if err != nil {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		return nil, stored, err
	}

	var active *activeNativeSession
	if err := AttachNativeSessionIO(
		sess, s.logDir, nativeprotocol.Provider(req.Agent), generation.Number, providerID,
		req.Prompt, !req.Interactive, false, s.eventBroker,
		func() string {
			if active == nil {
				return ""
			}
			return active.terminalReason()
		},
		func(transport nativeprotocol.Transport) {
			active = s.setNativeTransport(sess.ID, transport)
			s.armNativeTimeout(sess.ID, generation.Number, active, req.EffectiveTimeout(), generation.CreatedAt)
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
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		s.finalizeV1Session(sess.ID, runtime.ExitResult{Code: -1, Err: err}, err)
		return nil, stored, durable.NewError(durable.CodeIndeterminate, op, "attach native transport", err)
	}
	log.Printf("[session %s] spawned durable internal session: agent=%s runtime=%s generation=%d", sess.ID, req.Agent, rt.Name(), generation.Number)
	return sess, stored, nil
}
