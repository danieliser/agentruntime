package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/danieliser/agentruntime/pkg/session/agentsessions"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func (s *Server) handleHealth(c *gin.Context) {
	available := make([]string, 0, len(s.runtimes))
	for name := range s.runtimes {
		available = append(available, name)
	}
	sort.Strings(available)
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"default_runtime":  s.runtime.Name(),
		"runtimes":         available,
	})
}

func (s *Server) handleCreateSession(c *gin.Context) {
	var req SessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
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

	// Check if resuming a session with a persistent volume
	var originalSession *session.Session
	if req.ResumeSession != "" {
		originalSession = s.sessions.Get(req.ResumeSession)
		if originalSession != nil && originalSession.VolumeName != "" {
			// Inherit persistence from the original session
			req.PersistSession = true
		}
	}

	resumeSessionID, err := s.lookupResumeSessionID(req.Agent, req.ResumeSession, originalSession)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Build the command.
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	spawnCmd := cmd
	if rt.Name() == "docker" && len(cmd) > 0 {
		spawnCmd = []string{cmd[0]}
	}

	// Create the session. Use caller-provided session ID if valid UUID.
	requestedID := req.SessionID
	if requestedID != "" {
		if _, err := uuid.Parse(requestedID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id must be a valid UUID"})
			return
		}
		if s.sessions.Get(requestedID) != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "session_id already exists"})
			return
		}
	}
	sess := session.NewSessionWithID(requestedID, req.TaskID, req.Agent, rt.Name(), req.Tags)
	if err := s.prepareSessionDir(sess, &req, workDir); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.sessions.Add(sess); err != nil {
		if errors.Is(err, session.ErrMaxSessions) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	// Determine volume name for persistence
	var volumeNameForSpawn string
	if req.PersistSession {
		if originalSession != nil && originalSession.VolumeName != "" {
			// Reuse the original session's volume for resume
			volumeNameForSpawn = originalSession.VolumeName
		} else {
			// Create a new volume for this session
			volumeNameForSpawn = "agentruntime-vol-" + sess.ID
		}
		sess.VolumeName = volumeNameForSpawn
	}

	// Spawn the process.
	ctx := context.Background()
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sess.SetRunning(handle)
	log.Printf("[session %s] spawned: agent=%s pid=%d cmd=%v", sess.ID, req.Agent, handle.PID(), cmd)

	// Close stdin for prompt-mode agents (claude -p, codex exec).
	// Interactive sessions keep stdin open so WS stdin frames can steer them.
	if !req.Interactive && handle.Stdin() != nil {
		handle.Stdin().Close()
	}

	// Create persistent log file for full chat log preservation.
	// Output is tee'd to both the replay buffer (for WS streaming) and the
	// log file (for permanent NDJSON record). The log file path is returned
	// in the session response so callers can retrieve it later.
	AttachSessionIO(sess, s.logDir)

	// Snapshot after SetRunning — the goroutine hasn't had a chance to call
	// SetCompleted yet, but we use Snapshot for correctness with the race detector.
	snap := sess.Snapshot()
	c.JSON(http.StatusCreated, SessionResponse{
		SessionID: snap.ID,
		TaskID:    snap.TaskID,
		Agent:     snap.AgentName,
		Runtime:   snap.RuntimeName,
		Status:    string(snap.State),
		WSURL:     sessionWSURL(c, snap.ID),
		LogURL:    sessionLogURL(c, snap.ID),
	})
}

func (s *Server) handleListSessions(c *gin.Context) {
	sessions := s.sessions.List()
	summaries := make([]SessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		snap := sess.Snapshot()
		summaries = append(summaries, SessionSummary{
			SessionID: snap.ID,
			TaskID:    snap.TaskID,
			Agent:     snap.AgentName,
			Runtime:   snap.RuntimeName,
			Status:    string(snap.State),
			CreatedAt: snap.CreatedAt,
			Tags:      snap.Tags,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].CreatedAt.Equal(summaries[j].CreatedAt) {
			return summaries[i].SessionID < summaries[j].SessionID
		}
		return summaries[i].CreatedAt.Before(summaries[j].CreatedAt)
	})
	c.JSON(http.StatusOK, summaries)
}

func (s *Server) handleGetSession(c *gin.Context) {
	sess := s.sessions.Get(c.Param("id"))
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.JSON(http.StatusOK, sess.Snapshot())
}

func (s *Server) handleGetSessionInfo(c *gin.Context) {
	sess := s.sessions.Get(c.Param("id"))
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	snap := sess.Snapshot()

	// Calculate uptime duration.
	uptime := ""
	if snap.EndedAt != nil {
		// Session is completed — use CreatedAt to EndedAt.
		uptime = formatDuration(snap.EndedAt.Sub(snap.CreatedAt))
	} else {
		// Session is still running — use CreatedAt to now.
		uptime = formatDuration(time.Since(snap.CreatedAt))
	}

	c.JSON(http.StatusOK, SessionInfo{
		SessionID:     snap.ID,
		TaskID:        snap.TaskID,
		Agent:         snap.AgentName,
		Runtime:       snap.RuntimeName,
		Status:        string(snap.State),
		CreatedAt:     snap.CreatedAt,
		EndedAt:       snap.EndedAt,
		ExitCode:      snap.ExitCode,
		SessionDir:    snap.SessionDir,
		VolumeName:    snap.VolumeName,
		LogFile:       session.LogFilePath(s.logDir, snap.ID),
		WSURL:         sessionWSURL(c, snap.ID),
		LogURL:        sessionLogURL(c, snap.ID),
		Uptime:        uptime,
		LastActivity:  snap.LastActivity,
		InputTokens:   snap.InputTokens,
		OutputTokens:  snap.OutputTokens,
		CostUSD:       snap.CostUSD,
		ToolCallCount: snap.ToolCallCount,
	})
}

func (s *Server) handleDeleteSession(c *gin.Context) {
	sess := s.sessions.Get(c.Param("id"))
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	_ = sess.Kill()
	sess.Replay.Close()
	sess.SetCompleted(-1)
	s.sessions.Remove(sess.ID)

	// Check if caller requested volume removal
	removeVolume := c.Query("remove_volume") == "true"
	if removeVolume && sess.VolumeName != "" && s.runtime.Name() == "docker" {
		// Try to remove the volume, but don't fail the deletion if it doesn't exist
		rt := s.RuntimeFor("docker")
		if rt != nil {
			dockerRT, ok := rt.(*runtime.DockerRuntime)
			if ok {
				_ = dockerRT.RemoveSessionVolume(context.Background(), sess.VolumeName)
			}
		}
	}

	snap := sess.Snapshot()
	c.JSON(http.StatusOK, gin.H{"id": snap.ID, "state": snap.State})
}

func (s *Server) handleGetLogs(c *gin.Context) {
	sess := s.sessions.Get(c.Param("id"))
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	cursor := int64(0)
	if cursorStr := c.Query("cursor"); cursorStr != "" {
		parsed, err := strconv.ParseInt(cursorStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cursor"})
			return
		}
		cursor = parsed
	}

	data, nextOffset := sess.Replay.ReadFrom(cursor)
	c.Header("Agentruntime-Log-Cursor", strconv.FormatInt(nextOffset, 10))
	c.Data(http.StatusOK, "text/plain", data)
}

func (s *Server) handleSessionWS(c *gin.Context) {
	sess := s.sessions.Get(c.Param("id"))
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if sess.Handle == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "session has no active process"})
		return
	}

	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Parse ?since= for replay offset. Default -1 (no replay).
	sinceOffset := int64(-1)
	if sinceStr := c.Query("since"); sinceStr != "" {
		if parsed, parseErr := strconv.ParseInt(sinceStr, 10, 64); parseErr == nil {
			sinceOffset = parsed
		}
	}

	b := bridge.New(conn, sess.Handle, sess.Replay, s.logDir, sess.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	b.Run(ctx, sess.ID, sinceOffset)
}

func (s *Server) handleGetLogFile(c *gin.Context) {
	id := c.Param("id")
	logPath, exists, err := session.ExistingLogFilePath(s.logDir, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log file lookup failed"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "log file not found"})
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.File(logPath)
}

func (s *Server) handleSessionHistory(c *gin.Context) {
	// Parse limit query parameter (default 50)
	limit := 50
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	// Scan the log directory for *.ndjson files
	entries, err := os.ReadDir(s.logDir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, []SessionHistoryEntry{})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read log directory"})
		return
	}

	var historyEntries []SessionHistoryEntry

	for _, entry := range entries {
		// Only process NDJSON files
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ndjson") {
			continue
		}

		// Extract session ID from filename (remove .ndjson extension)
		sessionID := strings.TrimSuffix(entry.Name(), ".ndjson")
		logPath := filepath.Join(s.logDir, entry.Name())

		// Get file info (size, mtime)
		info, err := os.Stat(logPath)
		if err != nil {
			continue // Skip files we can't stat
		}

		// Parse the last few lines to extract result event data
		histEntry := parseSessionLogTail(logPath, sessionID, info)
		if histEntry != nil {
			historyEntries = append(historyEntries, *histEntry)
		}
	}

	// Sort by EndedAt descending (newest first)
	sort.Slice(historyEntries, func(i, j int) bool {
		if historyEntries[i].EndedAt == nil {
			return false
		}
		if historyEntries[j].EndedAt == nil {
			return true
		}
		return historyEntries[i].EndedAt.After(*historyEntries[j].EndedAt)
	})

	// Apply limit
	if len(historyEntries) > limit {
		historyEntries = historyEntries[:limit]
	}

	c.JSON(http.StatusOK, historyEntries)
}

// SpawnSession implements chat.SessionSpawner. It creates and starts a session
// using the same pipeline as handleCreateSession, without HTTP context.
func (s *Server) SpawnSession(ctx context.Context, req SessionRequest) (*session.Session, error) {
	rt := s.RuntimeFor(req.Runtime)
	if rt == nil {
		return nil, fmt.Errorf("unknown runtime: %s", req.Runtime)
	}

	ag := s.agents.Get(req.Agent)
	if ag == nil {
		return nil, fmt.Errorf("unknown agent: %s", req.Agent)
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
	log.Printf("[session %s] spawned (chat): agent=%s pid=%d", sess.ID, req.Agent, handle.PID())

	if !req.Interactive && handle.Stdin() != nil {
		handle.Stdin().Close()
	}

	AttachSessionIO(sess, s.logDir)
	return sess, nil
}

func effectiveWorkDir(workDir string, mounts []Mount) string {
	if workDir != "" {
		return workDir
	}
	for _, mount := range mounts {
		if mount.Mode != "ro" && mount.Host != "" {
			return mount.Host
		}
	}
	if len(mounts) > 0 {
		return mounts[0].Host
	}
	return ""
}

func formatDuration(d time.Duration) string {
	totalSecs := int(d.Seconds())
	hours := totalSecs / 3600
	mins := (totalSecs % 3600) / 60
	secs := totalSecs % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, mins, secs)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm%ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

func httpScheme(c *gin.Context) string {
	if c.Request.TLS != nil {
		return "https"
	}
	return "http"
}

func websocketScheme(c *gin.Context) string {
	if c.Request.TLS != nil {
		return "wss"
	}
	return "ws"
}

func sessionWSURL(c *gin.Context, sessionID string) string {
	return websocketScheme(c) + "://" + c.Request.Host + "/ws/sessions/" + url.PathEscape(sessionID)
}

func sessionLogURL(c *gin.Context, sessionID string) string {
	return httpScheme(c) + "://" + c.Request.Host + "/sessions/" + url.PathEscape(sessionID) + "/logs"
}

func (s *Server) lookupResumeSessionID(agentName, sessionID string, original *session.Session) (string, error) {
	if sessionID == "" {
		return "", nil
	}

	// Prefer the claude_session_id tag captured from result events.
	// This works for Docker where filesystem scanning can't reach the named volume.
	if original != nil {
		snap := original.Snapshot()
		if snap.Tags != nil {
			if claudeID, ok := snap.Tags["claude_session_id"]; ok && claudeID != "" {
				return claudeID, nil
			}
		}
	}

	// Fall back to filesystem scanning (works for local runtime).
	var (
		args []string
		err  error
	)

	switch agentName {
	case "claude":
		args, err = agentsessions.ClaudeResumeArgs(s.dataDir, sessionID)
	case "codex":
		args, err = agentsessions.CodexResumeArgs(s.dataDir, sessionID)
	default:
		return "", fmt.Errorf("resume_session is not supported for agent: %s", agentName)
	}
	if err != nil {
		return "", err
	}

	return resumeSessionIDFromArgs(args)
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

// SessionHistoryEntry represents a completed/failed session in the history.
type SessionHistoryEntry struct {
	SessionID    string     `json:"session_id"`
	Agent        string     `json:"agent,omitempty"`
	Status       string     `json:"status,omitempty"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	LogFile      string     `json:"log_file,omitempty"`
	InputTokens  int        `json:"input_tokens,omitempty"`
	OutputTokens int        `json:"output_tokens,omitempty"`
	CostUSD      float64    `json:"cost_usd,omitempty"`
	ToolCalls    int        `json:"tool_calls,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
}

// parseSessionLogTail parses the tail of an NDJSON log file to extract session metadata.
func parseSessionLogTail(logPath, sessionID string, fileInfo os.FileInfo) *SessionHistoryEntry {
	entry := &SessionHistoryEntry{
		SessionID: sessionID,
		LogFile:   logPath,
		FileSize:  fileInfo.Size(),
	}

	// Open the file and read the last few lines
	f, err := os.Open(logPath)
	if err != nil {
		return entry
	}
	defer f.Close()

	// Read file in reverse to find the result event (should be near the end)
	// For now, we'll scan forward and keep parsing lines until we find useful data
	var lastResultEvent map[string]interface{}
	var firstTimestamp *time.Time

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var evt map[string]interface{}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}

		// Extract timestamp from first event to set CreatedAt
		if firstTimestamp == nil {
			if timestampVal, ok := evt["timestamp"]; ok {
				if timestampInt, ok := timestampVal.(float64); ok {
					ts := time.UnixMilli(int64(timestampInt))
					firstTimestamp = &ts
					entry.CreatedAt = firstTimestamp
				}
			}
		}

		// Look for result events
		if eventType, ok := evt["type"].(string); ok {
			if eventType == "result" {
				lastResultEvent = evt
				// Update timestamp for EndedAt
				if timestampVal, ok := evt["timestamp"]; ok {
					if timestampInt, ok := timestampVal.(float64); ok {
						ts := time.UnixMilli(int64(timestampInt))
						entry.EndedAt = &ts
					}
				}
			}
		}
	}

	// Parse result event data if found
	if lastResultEvent != nil {
		if data, ok := lastResultEvent["data"].(map[string]interface{}); ok {
			// Extract status
			if status, ok := data["status"].(string); ok {
				entry.Status = status
			}
			// Extract exit code
			if exitCode, ok := data["exit_code"].(float64); ok {
				code := int(exitCode)
				entry.ExitCode = &code
			}
			// Extract usage metrics
			if usage, ok := data["usage"].(map[string]interface{}); ok {
				if input, ok := usage["input_tokens"].(float64); ok {
					entry.InputTokens = int(input)
				}
				if output, ok := usage["output_tokens"].(float64); ok {
					entry.OutputTokens = int(output)
				}
				if cost, ok := usage["cost_usd"].(float64); ok {
					entry.CostUSD = cost
				}
			}
			// Extract tool call count
			if toolCalls, ok := data["tool_calls"].(float64); ok {
				entry.ToolCalls = int(toolCalls)
			}
		}
	}

	return entry
}

