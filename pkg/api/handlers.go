package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/danieliser/agentruntime/pkg/session/agentsessions"
)

func (s *Server) handleHealth(c *gin.Context) {
	available := make([]string, 0, len(s.runtimes))
	runtimeStatus := make(map[string]string, len(s.runtimes))
	runtimeChecks := s.readiness.snapshot(time.Now().UTC())
	ready := true
	for name := range s.runtimes {
		available = append(available, name)
		snapshot := runtimeChecks[name]
		runtimeStatus[name] = snapshot.Status
		if snapshot.Stale {
			runtimeStatus[name] = "stale"
		}
		if snapshot.Status != "ready" || snapshot.Stale {
			ready = false
			if snapshot.Status == "checking" {
				runtimeStatus[name] = "unavailable"
			}
		}
	}
	sort.Strings(available)
	statusCode := http.StatusOK
	status := "ok"
	if !ready {
		statusCode = http.StatusServiceUnavailable
		status = "error"
	}
	c.JSON(statusCode, gin.H{
		"status":          status,
		"version":         s.version,
		"default_runtime": s.runtime.Name(),
		"runtimes":        available,
		"runtime_status":  runtimeStatus,
		"runtime_checks":  runtimeChecks,
	})
}

func (s *Server) createSession(c *gin.Context, req SessionRequest) {
	if !s.beginAdmission() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": apiErrorEnvelope{Code: durable.CodeInvalidState, Message: errAdmissionClosed.Error()}})
		return
	}
	defer s.endAdmission()
	if req.Agent == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent is required"})
		return
	}
	if !req.Interactive && req.Prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}
	// Resolve runtime — use requested or default.
	rt := s.RuntimeFor(req.Runtime)
	if rt == nil {
		available := make([]string, 0, len(s.runtimes))
		for name := range s.runtimes {
			available = append(available, name)
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":     fmt.Sprintf("unknown runtime: %s", req.Runtime),
			"available": available,
		})
		return
	}
	configureDockerProviderPersistence(&req, rt.Name())
	resolvedPolicy, err := resolveExecutionPolicy(&req, rt.Name())
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if _, err := resolveStructuredOutput(&req); err != nil {
		writeDurableError(c, err)
		return
	}

	mounts := req.EffectiveMounts()
	workDir := effectiveWorkDir(req.WorkDir, mounts)

	// Validate the working directory
	if workDir != "" {
		if err := session.ValidateWorkDir(workDir); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_work_dir", "message": err.Error()})
			return
		}
	}

	// Look up the agent.
	ag := s.agents.Get(req.Agent)
	if ag == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown agent: %s", req.Agent)})
		return
	}

	if err := validateContextMode(&req, rt.Name()); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Ensure the agent-specific config block exists so the materializer
	// can set up credentials, config files, and MCP servers. Callers can
	// send an empty block (e.g. "codex": {}) — omitting it entirely is
	// also fine; we infer the default here.
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

	// Validate and prepare team config.
	if req.Team != nil {
		if req.Team.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "team.name is required"})
			return
		}
		if req.Team.AgentName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "team.agent_name is required for team sessions"})
			return
		}
		if req.Agent != "claude" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "team config is only supported for claude agent"})
			return
		}
		// Validate team directory exists on disk.
		teamConfigPath := filepath.Join(os.Getenv("HOME"), ".claude", "teams", req.Team.Name, "config.json")
		if _, err := os.Stat(teamConfigPath); os.IsNotExist(err) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "team not found",
				"team":    req.Team.Name,
				"message": "orchestrator must scaffold team directory before spawning",
			})
			return
		}
		// Auto-generate agent_id if not provided.
		if req.Team.AgentID == "" {
			req.Team.AgentID = req.Team.AgentName + "@" + req.Team.Name
		}
		// Auto-tag session with team metadata.
		if req.Tags == nil {
			req.Tags = make(map[string]string)
		}
		req.Tags["team"] = req.Team.Name
		req.Tags["team_agent"] = req.Team.AgentName
	}

	// Check if resuming a session with a persistent volume
	var originalSession *session.Session
	if req.ResumeSession != "" {
		originalSession = s.sessions.Get(req.ResumeSession)
		if originalSession != nil && originalSession.VolumeName != "" {
			// Inherit persistence from the original session
			req.PersistSession = true
		}
	}

	resumeSession, err := s.resolveResumeSession(c.Request.Context(), req.Agent, req.ResumeSession, originalSession)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateResolvedResumeState(rt.Name(), req.ResumeSession, resumeSession); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if resumeSession.VolumeName != "" {
		req.PersistSession = true
	}
	resumeSessionID := resumeSession.ProviderID

	// ACT-1001: native HTTP admission uses the same resolver as internal
	// admission and generation resume. Provider controls must not be rebuilt
	// independently in this handler.
	var cmd []string
	if nativeV1Agent(req.Agent) {
		resolved, resolveErr := resolveNativeExecution(req, ag, workDir, resumeSessionID)
		if resolveErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": resolveErr.Error()})
			return
		}
		cmd = resolved.Command
	} else {
		agCfg := agent.AgentConfig{
			Model: req.Model, WorkDir: workDir, Env: req.Env,
			Interactive: req.Interactive, ResumeSessionID: resumeSessionID,
			Effort: req.Effort, Fast: req.Fast,
		}
		prompt := req.Prompt
		if req.Interactive {
			prompt = ""
		}
		cmd, err = ag.BuildCmd(prompt, agCfg)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	spawnCmd := runtimeSpawnCommand(cmd, rt.Name(), req.Agent)

	// Create the session. Use caller-provided session ID if valid UUID.
	requestedID := req.SessionID
	if requestedID != "" {
		if _, err := uuid.Parse(requestedID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id must be a valid UUID"})
			return
		}
	}
	if err := s.checkRuntimeAdmission(c.Request.Context(), req, rt); err != nil {
		writeDurableError(c, err)
		return
	}
	result, err := s.admitV1Session(c.Request.Context(), req, rt.Name())
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if !result.Created {
		s.writeV1Session(c, http.StatusOK, result.Session)
		return
	}
	admitted := result.Session
	if nativeV1Agent(req.Agent) {
		req.AdmittedSessionID = admitted.ID
		s.launchAdmittedNativeSession(req)
		s.writeV1Session(c, http.StatusCreated, admitted)
		return
	}
	requestedID = admitted.ID
	sess := session.NewSessionWithID(requestedID, req.TaskID, req.Agent, rt.Name(), req.Tags)
	sess.Contamination = agent.KnownContamination(req.Agent, req.Context == "clean")
	if err := s.prepareSessionDir(sess, &req, workDir); err != nil {
		s.writeAdmittedFailure(c, admitted.ID, err)
		return
	}
	if err := s.sessions.Add(sess); err != nil {
		s.writeAdmittedFailure(c, admitted.ID, err)
		return
	}

	// Determine volume name for persistence
	var volumeNameForSpawn string
	if req.PersistSession {
		if resumeSession.VolumeName != "" {
			volumeNameForSpawn = resumeSession.VolumeName
		} else if originalSession != nil && originalSession.VolumeName != "" {
			// Reuse the original session's volume for resume
			volumeNameForSpawn = originalSession.VolumeName
		} else {
			// Create a new volume for this session
			volumeNameForSpawn = "agentruntime-vol-" + sess.ID
		}
		sess.VolumeName = volumeNameForSpawn
	}

	// Spawn the process.
	generationNumber := admitted.ActiveGeneration + 1
	lifecycleCtx, lifecycleCancel := context.WithTimeout(context.Background(), 10*time.Second)
	admitted, err = s.durableStore.TransitionSession(lifecycleCtx, durable.TransitionSessionParams{
		SessionID: admitted.ID, From: durable.StateCreated, To: durable.StateStarting, At: time.Now().UTC(),
	})
	lifecycleCancel()
	if err != nil {
		s.sessions.Remove(sess.ID)
		s.writeAdmittedFailure(c, admitted.ID, err)
		return
	}
	ctx := context.Background()
	spawnConfig := runtime.SpawnConfig{
		SessionID:           sess.ID,
		Generation:          generationNumber,
		IdempotencyKey:      admitted.IdempotencyKey,
		RequestHash:         admitted.RequestHash,
		ExecutionPolicyHash: resolvedPolicy.Hash,
		AgentName:           req.Agent,
		Cmd:                 spawnCmd,
		Prompt:              req.Prompt,
		Model:               req.Model,
		Env:                 req.Env,
		WorkDir:             workDir,
		TaskID:              req.TaskID,
		Request:             &req,
		SessionDir:          &sess.SessionDir,
		VolumeName:          volumeNameForSpawn,
		PTY:                 req.PTY,
		SandboxProfile:      requestSandboxProfile(rt.Name(), nativeV1Agent(req.Agent), req),
	}
	handle, err := rt.Spawn(ctx, spawnConfig)
	if err != nil {
		s.sessions.Remove(sess.ID)
		s.writeAdmittedFailure(c, admitted.ID, classifyRuntimeFailure(err))
		return
	}

	sess.SetRunning(handle)
	lifecycleCtx, lifecycleCancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer lifecycleCancel()
	runtimeID := runtimeGenerationIdentity(handle, rt.Name(), sess.ID, generationNumber)
	if runtimeID == "" {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		s.writeAdmittedFailure(c, admitted.ID, durable.NewError(durable.CodeIndeterminate, "activate_v1_generation", "runtime did not expose a reconstructable identity", nil))
		return
	}
	nativeGeneration := nativeV1Agent(req.Agent)
	generation, err := s.durableStore.CreateGeneration(lifecycleCtx, durable.CreateGenerationParams{
		SessionID: sess.ID, Runtime: rt.Name(), ContainerID: runtimeID,
		ImageReference:  runtimeGenerationImageReference(handle, req, rt.Name()),
		ImageDigest:     runtimeGenerationImageDigest(handle),
		SandboxProfile:  requestSandboxProfile(rt.Name(), nativeGeneration, req),
		DockerLogDriver: generationDockerLogDriver(rt.Name(), nativeGeneration), CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		s.writeAdmittedFailure(c, admitted.ID, err)
		return
	}
	if _, err := s.durableStore.TransitionGeneration(lifecycleCtx, durable.TransitionGenerationParams{
		SessionID: sess.ID, Generation: generation.Number,
		From: durable.GenerationStarting, To: durable.GenerationRunning, At: time.Now().UTC(),
	}); err != nil {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		s.writeAdmittedFailure(c, admitted.ID, err)
		return
	}
	admitted, err = s.durableStore.TransitionSession(lifecycleCtx, durable.TransitionSessionParams{
		SessionID: sess.ID, From: durable.StateStarting, To: durable.StateRunning, At: time.Now().UTC(),
	})
	if err != nil {
		_ = handle.Kill()
		s.sessions.Remove(sess.ID)
		s.writeAdmittedFailure(c, admitted.ID, err)
		return
	}
	_, nativeStdio := handle.(runtime.NativeStdioHandle)
	usesNativeTransport := nativeStdio && (req.Agent == string(nativeprotocol.ProviderClaude) || req.Agent == string(nativeprotocol.ProviderCodex))
	log.Printf("[session %s] spawned: agent=%s pid=%d cmd=%v", sess.ID, req.Agent, handle.PID(), agent.RedactPrompt(cmd, req.Prompt))

	// Close stdin for prompt-mode agents (claude -p, codex exec).
	// Interactive sessions keep stdin open so WS stdin frames can steer them.
	if !req.Interactive && handle.Stdin() != nil && !usesNativeTransport {
		handle.Stdin().Close()
	}

	// Create persistent log file for full chat log preservation.
	// Output is tee'd to both the replay buffer (for WS streaming) and the
	// log file (for permanent NDJSON record). The log file path is returned
	// in the session response so callers can retrieve it later.
	if usesNativeTransport {
		var active activeNativeSessionRef
		if err := AttachNativeSessionIO(
			sess, s.logDir, nativeprotocol.Provider(req.Agent), admitted.ActiveGeneration,
			resumeSessionID, req.Prompt, diagnosticRedactions(req), !req.Interactive,
			false, nativePolicy(req), req.StructuredOutput, s.eventBroker,
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
				s.armNativeTimeout(sess.ID, admitted.ActiveGeneration, current, req.EffectiveTimeout(), admitted.UpdatedAt)
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
			classified := classifyNativeBootstrapFailure(rt, spawnConfig, err)
			log.Printf("[session %s] attach native event transport failed: %v", sess.ID, classified)
			_ = handle.Kill()
			s.sessions.Remove(sess.ID)
			s.writeAdmittedFailure(c, admitted.ID, classified)
			return
		}
	} else {
		AttachSessionIO(sess, s.logDir, diagnosticRedactions(req), func(result runtime.ExitResult) {
			s.finalizeV1Session(sess.ID, result)
		})
	}

	// Snapshot after SetRunning — the goroutine hasn't had a chance to call
	// SetCompleted yet, but we use Snapshot for correctness with the race detector.
	s.writeV1Session(c, http.StatusCreated, admitted)
}

func runtimeSpawnCommand(command []string, runtimeName, agentName string) []string {
	if runtimeName != "docker" || len(command) == 0 {
		return command
	}
	if nativeV1Agent(agentName) {
		return command
	}
	return []string{command[0]}
}

// SpawnSession implements chat.SessionSpawner. It creates and starts a session
// using the same pipeline as handleCreateSession, without HTTP context.
func (s *Server) SpawnSession(ctx context.Context, req SessionRequest) (*session.Session, error) {
	if !s.beginAdmission() {
		return nil, errAdmissionClosed
	}
	defer s.endAdmission()
	if s.durableStore != nil && s.eventBroker != nil && nativeV1Agent(req.Agent) {
		sess, _, err := s.spawnDurableSession(ctx, req)
		return sess, err
	}
	rt := s.RuntimeFor(req.Runtime)
	if rt == nil {
		return nil, fmt.Errorf("unknown runtime: %s", req.Runtime)
	}

	ag := s.agents.Get(req.Agent)
	if ag == nil {
		return nil, fmt.Errorf("unknown agent: %s", req.Agent)
	}

	if err := validateContextMode(&req, rt.Name()); err != nil {
		return nil, err
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

	var originalSession *session.Session
	if req.ResumeSession != "" {
		originalSession = s.sessions.Get(req.ResumeSession)
		if originalSession != nil && originalSession.VolumeName != "" {
			req.PersistSession = true
		}
	}

	resumeSessionID, err := s.lookupResumeSessionID(req.Agent, req.ResumeSession, originalSession)
	if err != nil {
		return nil, fmt.Errorf("lookup resume session: %w", err)
	}

	mounts := req.EffectiveMounts()
	workDir := effectiveWorkDir(req.WorkDir, mounts)

	agCfg := agent.AgentConfig{
		WorkDir:         workDir,
		Env:             req.Env,
		Interactive:     req.Interactive,
		ResumeSessionID: resumeSessionID,
	}
	prompt := req.Prompt
	if req.Interactive {
		prompt = ""
	}
	cmd, err := ag.BuildCmd(prompt, agCfg)
	if err != nil {
		return nil, fmt.Errorf("build cmd: %w", err)
	}

	spawnCmd := cmd
	if rt.Name() == "docker" && len(cmd) > 0 {
		spawnCmd = []string{cmd[0]}
	}

	sess := session.NewSessionWithID(req.SessionID, req.TaskID, req.Agent, rt.Name(), req.Tags)
	sess.Contamination = agent.KnownContamination(req.Agent, req.Context == "clean")
	if err := s.prepareSessionDir(sess, &req, workDir); err != nil {
		return nil, fmt.Errorf("prepare session dir: %w", err)
	}
	if err := s.sessions.Add(sess); err != nil {
		return nil, fmt.Errorf("add session: %w", err)
	}

	var volumeNameForSpawn string
	if req.PersistSession {
		if originalSession != nil && originalSession.VolumeName != "" {
			volumeNameForSpawn = originalSession.VolumeName
		} else {
			volumeNameForSpawn = "agentruntime-vol-" + sess.ID
		}
		sess.VolumeName = volumeNameForSpawn
	}

	handle, err := rt.Spawn(ctx, runtime.SpawnConfig{
		SessionID:  sess.ID,
		AgentName:  req.Agent,
		Cmd:        spawnCmd,
		Prompt:     req.Prompt,
		Model:      req.Model,
		Env:        req.Env,
		WorkDir:    workDir,
		TaskID:     req.TaskID,
		Request:    &req,
		SessionDir: &sess.SessionDir,
		VolumeName: volumeNameForSpawn,
		PTY:        req.PTY,
	})
	if err != nil {
		s.sessions.Remove(sess.ID)
		return nil, fmt.Errorf("spawn: %w", err)
	}

	sess.SetRunning(handle)
	log.Printf("[session %s] spawned (chat): agent=%s pid=%d resume=%q", sess.ID, req.Agent, handle.PID(), req.ResumeSession)

	if !req.Interactive && handle.Stdin() != nil {
		handle.Stdin().Close()
	}

	AttachSessionIO(sess, s.logDir, diagnosticRedactions(req))
	return sess, nil
}

// validateContextMode normalizes req.Context and enforces clean-context
// preconditions. Shared by the HTTP and chat/internal spawn paths so no
// session can request an isolation mode its runtime cannot honor.
func validateContextMode(req *SessionRequest, runtimeName string) error {
	switch req.Context {
	case "", "clean":
	default:
		return fmt.Errorf("unknown context mode: %q (valid: \"clean\")", req.Context)
	}
	if req.Context != "clean" {
		return nil
	}
	// Clean context forces auto-discovery off: the whole point is that no
	// host config reaches the agent.
	req.AutoDiscover = false
	// local-pipe predates the sidecar and has no clean-context materialization.
	if runtimeName == "local-pipe" {
		return fmt.Errorf("context %q is not supported on the local-pipe runtime: it bypasses the sidecar isolation layer", req.Context)
	}
	// The docker agent image bundles only claude and codex; grok/cursor clean
	// context would fail inside the container with a missing binary and no
	// host auth to materialize.
	if runtimeName == "docker" && (req.Agent == "grok" || req.Agent == "cursor") {
		return fmt.Errorf("context %q for agent %q is not supported on the docker runtime: the agent image bundles neither the CLI nor its credentials", req.Context, req.Agent)
	}
	return nil
}

func effectiveWorkDir(workDir string, mounts []Mount) string {
	if workDir != "" {
		return workDir
	}
	// Only derive workDir from bind-type mounts — volume mounts carry a volume
	// name in Host, not a filesystem path, so they must never be stat-checked.
	for _, mount := range mounts {
		if mount.Type == "volume" {
			continue
		}
		if mount.Mode != "ro" && mount.Host != "" {
			return mount.Host
		}
	}
	for _, mount := range mounts {
		if mount.Type == "volume" {
			continue
		}
		if mount.Host != "" {
			return mount.Host
		}
	}
	return ""
}

func (s *Server) lookupResumeSessionID(agentName, sessionID string, original *session.Session) (string, error) {
	resolved, err := s.resolveResumeSession(context.Background(), agentName, sessionID, original)
	return resolved.ProviderID, err
}

func resumeSessionIDFromArgs(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}

	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--session", "--session-id":
			if args[i+1] == "" {
				return "", fmt.Errorf("resume args contain empty session id")
			}
			return args[i+1], nil
		}
	}

	return "", fmt.Errorf("resume args missing session id")
}

func (s *Server) prepareSessionDir(sess *session.Session, req *SessionRequest, workDir string) error {
	if sess == nil || req == nil {
		return nil
	}

	switch req.Agent {
	case "claude":
		credentialsPath := ""
		if req.Claude != nil {
			credentialsPath = req.Claude.CredentialsPath
		}
		if workDir == "" {
			workDir = "/"
		}
		sessionDir, err := agentsessions.InitClaudeSessionDir(s.dataDir, sess.ID, workDir, credentialsPath)
		if err != nil {
			return fmt.Errorf("prepare claude session dir: %w", err)
		}
		sess.SessionDir = sessionDir
	case "codex":
		sessionDir, err := agentsessions.InitCodexSessionDir(s.dataDir, sess.ID)
		if err != nil {
			return fmt.Errorf("prepare codex session dir: %w", err)
		}
		sess.SessionDir = sessionDir
	}

	return nil
}
