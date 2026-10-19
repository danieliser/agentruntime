package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
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
	var resume v1ResumeRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&resume); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "decode resume request", err))
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if strings.TrimSpace(resume.Prompt) == "" || key == "" {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "resume prompt and Idempotency-Key header are required", nil))
		return
	}
	if !s.beginAdmission() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": apiErrorEnvelope{Code: durable.CodeInvalidState, Message: errAdmissionClosed.Error()}})
		return
	}
	defer s.endAdmission()
	unlock := s.sessionLocks.lock(c.Param("id"))
	defer unlock()
	stored, repeated, err := s.resumeSessionLocked(c.Request.Context(), c.Param("id"), key, resume, false)
	if err != nil {
		writeDurableError(c, err)
		return
	}
	status := http.StatusAccepted
	if repeated {
		status = http.StatusOK
	}
	s.writeV1Session(c, status, stored)
}

// resumeSessionLocked is the canonical native generation restart. Callers hold
// the session lock and an admission lease, including the follow-up input path.
func (s *Server) resumeSessionLocked(ctx context.Context, sessionID, idempotencyKey string, resume v1ResumeRequest, wake bool) (result durable.Session, repeated bool, resultErr error) {
	const op = "resume_v1_session"
	if s.durableStore == nil || s.eventBroker == nil {
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "durable session services unavailable", nil)
	}
	stored, err := s.durableStore.GetSession(ctx, sessionID)
	if err != nil {
		return durable.Session{}, false, err
	}
	activeGeneration, err := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
	if err != nil {
		return durable.Session{}, false, err
	}
	control, err := resumeControlParams(stored, activeGeneration.Number, idempotencyKey, resume)
	if err != nil {
		return durable.Session{}, false, err
	}
	if wake {
		control, err = s.nativeControlParamsContext(ctx, stored.ID, idempotencyKey, "prompt", map[string]any{"text": resume.Prompt})
		if err != nil {
			return durable.Session{}, false, err
		}
	}
	probe, err := s.probeSessionControl(ctx, control)
	if err != nil {
		return durable.Session{}, false, err
	}
	if probe {
		return stored, true, nil
	}
	reopeningCompleted := wake && stored.State == durable.StateCompleted
	previous := activeGeneration
	if wake {
		status := s.sessionContinuation(ctx, stored)
		if !status.Continuable {
			return durable.Session{}, false, durable.NewError(durable.CodeInvalidState, op, status.Reason, nil)
		}
	} else if previous.State != durable.GenerationLost || stored.State.Terminal() {
		return durable.Session{}, false, durable.NewError(durable.CodeInvalidState, op, "active generation is not durably lost or session is terminal", nil)
	}
	if previous.ProviderID == "" {
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "lost generation has no recoverable provider session identity", nil)
	}
	var request SessionRequest
	if err := json.Unmarshal(stored.RequestManifest, &request); err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "decode stored request manifest", err)
	}
	if err := applyResumeSecrets(&request, stored.SecretGrants, resume.Env); err != nil {
		return durable.Session{}, false, err
	}
	request.SessionID, request.Agent, request.Runtime, request.Prompt = stored.ID, stored.Agent, stored.Runtime, resume.Prompt
	rt := s.RuntimeFor(stored.Runtime)
	if rt == nil {
		return durable.Session{}, false, durable.NewError(durable.CodeInvalidState, op, "stored runtime is unavailable", nil)
	}
	if _, err := resolveExecutionPolicy(&request, rt.Name()); err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "stored execution policy is no longer enforceable", err)
	}
	if err := validateProviderPermissions(&request, rt.Name()); err != nil {
		return durable.Session{}, false, err
	}
	if _, err := resolveStructuredOutput(&request); err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "stored structured-output contract is no longer enforceable", err)
	}
	containerLease, err := resolveContainerLease(request, rt.Name(), true)
	if err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "stored container lease is no longer enforceable", err)
	}
	ag := s.agents.Get(stored.Agent)
	if ag == nil {
		return durable.Session{}, false, durable.NewError(durable.CodeInvalidState, op, "stored agent is unavailable", nil)
	}
	if err := validateContextMode(&request, rt.Name()); err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeInvalidArgument, op, "validate stored context", err)
	}
	mounts := request.EffectiveMounts()
	workDir := effectiveWorkDir(request.WorkDir, mounts)
	request.SessionID = stored.ID
	resolved, err := resolveNativeExecution(request, ag, workDir, previous.ProviderID)
	if err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeInvalidArgument, op, "resolve provider resume command", err)
	}
	command := resolved.Command
	command = runtimeSpawnCommand(command, rt.Name(), stored.Agent)
	if existing := s.sessions.Get(stored.ID); existing != nil {
		if existing.Snapshot().State == session.StateRunning {
			return durable.Session{}, false, durable.NewError(durable.CodeInvalidState, op, "provider turn is still active; use steer instead of prompt", nil)
		}
		s.sessions.Remove(stored.ID)
	}
	sess := session.NewSessionWithID(stored.ID, request.TaskID, stored.Agent, stored.Runtime, request.Tags)
	if err := s.prepareSessionDir(sess, &request, workDir); err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "prepare resumed session files", err)
	}
	if reopeningCompleted {
		stored, err = s.durableStore.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: stored.ID, From: durable.StateCompleted, To: durable.StateStarting, At: time.Now().UTC()})
		if err != nil {
			return durable.Session{}, false, err
		}
	}
	// Once reopening is durable, every failure must acquire terminal proof.
	// Use a fresh context: a disconnected caller must not strand admission.
	settleWakeFailure := wake
	if wake {
		defer func() {
			if resultErr == nil || !settleWakeFailure {
				return
			}
			settleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			latest, lookupErr := s.durableStore.GetSession(settleCtx, stored.ID)
			if lookupErr != nil {
				resultErr = errors.Join(resultErr, lookupErr)
				return
			}
			if latest.State.Terminal() {
				return
			}
			_, settleErr := s.settleAdmittedSession(settleCtx, stored.ID, durable.StateFailed, "failed", resultErr)
			resultErr = errors.Join(resultErr, settleErr)
		}()
	}
	begin, err := s.eventBroker.BeginControl(ctx, control)
	if err != nil {
		return durable.Session{}, false, err
	}
	if begin.AlreadyDispatched {
		return stored, true, nil
	}
	if err := s.sessions.Add(sess); err != nil {
		return durable.Session{}, false, durable.NewError(durable.CodeInvalidState, op, "register resumed session", err)
	}
	nextGeneration := stored.ActiveGeneration + 1
	volumeName := ""
	if request.PersistSession {
		rootID, persistent, rootErr := s.durableProviderStateRoot(ctx, stored)
		if rootErr != nil || !persistent {
			s.sessions.Remove(stored.ID)
			return durable.Session{}, false, durable.NewError(durable.CodeInvalidState, op, "persistent provider state is unavailable", rootErr)
		}
		volumeName = "agentruntime-vol-" + rootID
		sess.VolumeName = volumeName
	}
	spawnConfig := runtime.SpawnConfig{
		DataDir:   s.dataDir,
		SessionID: stored.ID, Generation: nextGeneration, IdempotencyKey: stored.IdempotencyKey,
		RequestHash: stored.RequestHash, ExecutionPolicyHash: manifestPolicyHash(stored.RequestManifest), AgentName: stored.Agent, Cmd: command, Prompt: request.Prompt,
		Model: request.Model, Env: request.Env, WorkDir: workDir, TaskID: request.TaskID,
		Request: &request, SessionDir: &sess.SessionDir, VolumeName: volumeName, PTY: request.PTY,
		SandboxProfile: requestSandboxProfile(rt.Name(), true, request),
	}
	handle, err := rt.Spawn(context.Background(), spawnConfig)
	if err != nil {
		s.sessions.Remove(stored.ID)
		return durable.Session{}, false, classifyRuntimeFailure(err)
	}
	sess.SetRunning(handle)
	runtimeID := runtimeGenerationIdentity(handle, rt.Name(), stored.ID, nextGeneration)
	if runtimeID == "" {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		return durable.Session{}, false, durable.NewError(durable.CodeIndeterminate, op, "resumed runtime did not expose a reconstructable identity", nil)
	}
	generation, err := s.durableStore.CreateGeneration(context.Background(), durable.CreateGenerationParams{
		SessionID: stored.ID, Runtime: rt.Name(), ContainerID: runtimeID,
		ImageReference: runtimeGenerationImageReference(handle, request, rt.Name()), ImageDigest: runtimeGenerationImageDigest(handle),
		SandboxProfile: requestSandboxProfile(rt.Name(), true, request),
		ProviderID:     previous.ProviderID, DockerLogDriver: generationDockerLogDriver(rt.Name(), true), CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		return durable.Session{}, false, err
	}
	if _, err := s.durableStore.TransitionGeneration(context.Background(), durable.TransitionGenerationParams{
		SessionID: stored.ID, Generation: generation.Number, From: durable.GenerationStarting,
		To: durable.GenerationRunning, At: time.Now().UTC(),
	}); err != nil {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		return durable.Session{}, false, err
	}
	if reopeningCompleted {
		if _, err := s.durableStore.TransitionSession(context.Background(), durable.TransitionSessionParams{SessionID: stored.ID, From: durable.StateStarting, To: durable.StateRunning, At: time.Now().UTC()}); err != nil {
			_ = handle.Kill()
			s.sessions.Remove(stored.ID)
			return durable.Session{}, false, err
		}
	}
	var dispatchErr error
	provider := nativeprotocol.Provider(stored.Agent)
	var active activeNativeSessionRef
	if err := AttachNativeSessionIO(
		sess, s.logDir, provider, generation.Number, previous.ProviderID, request.Prompt,
		diagnosticRedactions(request), !request.Interactive && !containerLease.Maintain, false, nativePolicy(request), request.StructuredOutput, s.eventBroker,
		func() string {
			current := active.Load()
			if current == nil {
				return ""
			}
			return current.terminalReason()
		},
		func(transport nativeprotocol.Transport) {
			_, dispatchErr = s.eventBroker.CompleteControl(context.Background(), control)
			var current *activeNativeSession
			if containerLease.Maintain {
				current = s.setMaintainedNativeTransport(
					stored.ID, transport, containerLease.IdleTTL, request.EffectiveTimeout(), generation.CreatedAt,
					func(expiring *activeNativeSession) {
						s.expireMaintainedNativeSession(stored.ID, generation.Number, expiring)
					},
					func(expiring *activeNativeSession) {
						s.expireNativeSession(stored.ID, generation.Number, expiring, request.EffectiveTimeout())
					},
				)
			} else {
				current = s.setNativeTransport(stored.ID, transport)
				current.setClosesAfterTurn(!request.Interactive)
			}
			active.Store(current)
			if !containerLease.Maintain {
				s.armNativeTimeout(stored.ID, generation.Number, current, request.EffectiveTimeout(), generation.CreatedAt)
			}
		},
		func(result runtime.ExitResult, streamErr error) {
			current := active.Load()
			s.clearNativeTransport(stored.ID, current)
			var override durable.SessionState
			var reason string
			if current != nil {
				override = current.terminalState()
				reason = current.terminalReceiptReason()
			}
			s.finalizeV1SessionClassified(stored.ID, result, override, reason, streamErr)
		},
		func() {
			if current := active.Load(); current != nil {
				current.turnCompleted()
			}
		},
		classifyNativeExitFailure(rt, spawnConfig),
	); err != nil {
		_ = handle.Kill()
		s.sessions.Remove(stored.ID)
		classified := classifyNativeBootstrapFailure(rt, spawnConfig, err)
		s.finalizeV1SessionClassifiedLocked(stored.ID, runtime.ExitResult{Code: -1, ErrorDetail: classified.Error(), Err: classified}, "", "", classified)
		return durable.Session{}, false, classified
	}
	settleWakeFailure = false // A live transport owns finalization after attachment.
	if dispatchErr != nil {
		return durable.Session{}, false, dispatchErr
	}
	resumed, err := s.durableStore.GetSession(ctx, stored.ID)
	if err != nil {
		return durable.Session{}, false, err
	}
	return resumed, false, nil
}

func resumeControlParams(stored durable.Session, generation int64, idempotencyKey string, resume v1ResumeRequest) (eventstream.ControlParams, error) {
	envKeys := make([]string, 0, len(resume.Env))
	for key := range resume.Env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	payload, err := json.Marshal(struct {
		Prompt  string   `json:"prompt"`
		EnvKeys []string `json:"env_keys,omitempty"`
	}{Prompt: resume.Prompt, EnvKeys: envKeys})
	if err != nil {
		return eventstream.ControlParams{}, durable.NewError(durable.CodeInvalidArgument, "resume_v1_session", "encode resume control", err)
	}
	return eventstream.ControlParams{
		SessionID: stored.ID, Generation: generation, IdempotencyKey: idempotencyKey,
		Timestamp: time.Now().UTC(), Kind: "resume", Payload: payload,
	}, nil
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
