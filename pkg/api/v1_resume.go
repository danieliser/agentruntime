package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type v1ResumeRequest struct {
	Prompt string            `json:"prompt"`
	Env    map[string]string `json:"env,omitempty"`
}

func (s *Server) handleV1ResumeSession(c *gin.Context) {
	const op = "resume_v1_session"
	if !s.beginAdmission() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": apiErrorEnvelope{Code: durable.CodeInvalidState, Message: errAdmissionClosed.Error()}})
		return
	}
	defer s.endAdmission()
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	if s.durableStore == nil || s.eventBroker == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "durable session services unavailable", nil))
		return
	}
	stored, err := s.durableStore.GetSession(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if stored.State.Terminal() {
		s.writeV1Session(c, http.StatusOK, stored)
		return
	}
	previous, err := s.durableStore.GetGeneration(c.Request.Context(), stored.ID, stored.ActiveGeneration)
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if previous.State != durable.GenerationLost {
		s.writeV1Session(c, http.StatusOK, stored)
		return
	}
	if previous.ProviderID == "" {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "lost generation has no recoverable provider session identity", nil))
		return
	}
	var resume v1ResumeRequest
	if err := c.ShouldBindJSON(&resume); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "decode resume request", err))
		return
	}
	if strings.TrimSpace(resume.Prompt) == "" {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "resume prompt is required", nil))
		return
	}
	var request SessionRequest
	if err := json.Unmarshal(stored.RequestManifest, &request); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "decode stored request manifest", err))
		return
	}
	if err := applyResumeSecrets(&request, stored.SecretGrants, resume.Env); err != nil {
		writeDurableError(c, err)
		return
	}
	request.SessionID, request.Agent, request.Runtime, request.Prompt = stored.ID, stored.Agent, stored.Runtime, resume.Prompt
	rt := s.RuntimeFor(stored.Runtime)
	if rt == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "stored runtime is unavailable", nil))
		return
	}
	ag := s.agents.Get(stored.Agent)
	if ag == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "stored agent is unavailable", nil))
		return
	}
	if err := validateContextMode(&request, rt.Name()); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "validate stored context", err))
		return
	}
	mounts := request.EffectiveMounts()
	workDir := effectiveWorkDir(request.WorkDir, mounts)
	command, err := ag.BuildCmd(request.Prompt, agent.AgentConfig{
		WorkDir: workDir, Env: request.Env, Interactive: request.Interactive,
		NativeStream: true, SessionID: stored.ID, ResumeSessionID: previous.ProviderID,
	})
	if err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "build provider resume command", err))
		return
	}
	if _, codex := ag.(*agent.CodexAgent); codex {
		command = []string{"codex", "app-server", "--listen", "stdio://"}
	}
	command = runtimeSpawnCommand(command, rt.Name(), stored.Agent)
	if existing := s.sessions.Get(stored.ID); existing != nil {
		if existing.Snapshot().State == session.StateRunning {
			s.writeV1Session(c, http.StatusOK, stored)
			return
		}
		s.sessions.Remove(stored.ID)
	}
	sess := session.NewSessionWithID(stored.ID, request.TaskID, stored.Agent, stored.Runtime, request.Tags)
	if err := s.prepareSessionDir(sess, &request, workDir); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "prepare resumed session files", err))
		return
	}
	if err := s.sessions.Add(sess); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "register resumed session", err))
		return
	}
	nextGeneration := stored.ActiveGeneration + 1
	volumeName := ""
	if request.PersistSession {
		volumeName = "agentruntime-vol-" + stored.ID
		sess.VolumeName = volumeName
	}
	handle, err := rt.Spawn(context.Background(), runtime.SpawnConfig{
		SessionID: stored.ID, Generation: nextGeneration, IdempotencyKey: stored.IdempotencyKey,
		RequestHash: stored.RequestHash, AgentName: stored.Agent, Cmd: command, Prompt: request.Prompt,
		Model: request.Model, Env: request.Env, WorkDir: workDir, TaskID: request.TaskID,
		Request: &request, SessionDir: &sess.SessionDir, VolumeName: volumeName, PTY: request.PTY,
	})
	if err != nil {
		s.sessions.Remove(stored.ID)
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "spawn resumed generation", err))
		return
	}
	sess.SetRunning(handle)
	runtimeID := runtimeGenerationIdentity(handle, rt.Name(), stored.ID, nextGeneration)
	if runtimeID == "" {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "resumed runtime did not expose a reconstructable identity", nil))
		return
	}
	generation, err := s.durableStore.CreateGeneration(context.Background(), durable.CreateGenerationParams{
		SessionID: stored.ID, Runtime: rt.Name(), ContainerID: runtimeID,
		ImageReference: resolvedImageReference(request, rt.Name()), ImageDigest: runtimeGenerationImageDigest(handle),
		SandboxProfile: runtimeSandboxProfile(rt.Name(), true),
		ProviderID:     previous.ProviderID, DockerLogDriver: generationDockerLogDriver(rt.Name(), true), CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		writeDurableError(c, err)
		return
	}
	if _, err := s.durableStore.TransitionGeneration(context.Background(), durable.TransitionGenerationParams{
		SessionID: stored.ID, Generation: generation.Number, From: durable.GenerationStarting,
		To: durable.GenerationRunning, At: time.Now().UTC(),
	}); err != nil {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		writeDurableError(c, err)
		return
	}
	provider := nativeprotocol.Provider(stored.Agent)
	var active *activeNativeSession
	if err := AttachNativeSessionIO(
		sess, s.logDir, provider, generation.Number, previous.ProviderID, request.Prompt,
		!request.Interactive, false, s.eventBroker,
		func() string {
			if active == nil {
				return ""
			}
			return active.terminalReason()
		},
		func(transport nativeprotocol.Transport) {
			active = s.setNativeTransport(stored.ID, transport)
		},
		func(result runtime.ExitResult, streamErr error) {
			s.clearNativeTransport(stored.ID, active)
			var override durable.SessionState
			if active != nil {
				override = active.terminalState()
			}
			s.finalizeV1SessionAs(stored.ID, result, override, streamErr)
		},
	); err != nil {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		s.finalizeV1Session(stored.ID, runtime.ExitResult{Code: -1, Err: err}, err)
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "attach resumed native transport", err))
		return
	}
	resumed, err := s.durableStore.GetSession(c.Request.Context(), stored.ID)
	if err != nil {
		writeDurableError(c, err)
		return
	}
	s.writeV1Session(c, http.StatusAccepted, resumed)
}

func applyResumeSecrets(request *SessionRequest, grants []string, supplied map[string]string) error {
	const op = "apply_resume_secrets"
	allowed := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		if strings.HasPrefix(grant, "request:") {
			return durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("resume requires a new create request to regrant nested secret %q", grant), nil)
		}
		allowed[grant] = struct{}{}
		if supplied[grant] == "" {
			return durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("resume environment grant %q is required", grant), nil)
		}
	}
	for name := range supplied {
		if _, ok := allowed[name]; !ok {
			return durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("resume environment key %q is not an approved secret grant", name), nil)
		}
	}
	if request.Env == nil && len(supplied) > 0 {
		request.Env = make(map[string]string, len(supplied))
	}
	for name, value := range supplied {
		request.Env[name] = value
	}
	return nil
}
