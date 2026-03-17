package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
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
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"runtime": s.runtime.Name(),
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
	if req.Runtime != "" && req.Runtime != s.runtime.Name() {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown runtime: %s", req.Runtime)})
		return
	}

	mounts := req.EffectiveMounts()
	workDir := effectiveWorkDir(req.WorkDir, mounts)

	// Look up the agent.
	ag := s.agents.Get(req.Agent)
	if ag == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown agent: %s", req.Agent)})
		return
	}

	resumeSessionID, err := s.lookupResumeSessionID(req.Agent, req.ResumeSession)
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
	if s.runtime.Name() == "docker" && len(cmd) > 0 {
		spawnCmd = []string{cmd[0]}
	}

	// Create the session.
	sess := session.NewSession(req.TaskID, req.Agent, s.runtime.Name(), req.Tags)
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

	// Spawn the process.
	ctx := context.Background()
	handle, err := s.runtime.Spawn(ctx, runtime.SpawnConfig{
		SessionID:  sess.ID,
		AgentName:  req.Agent,
		Cmd:        spawnCmd,
		Prompt:     req.Prompt,
		Env:        req.Env,
		WorkDir:    workDir,
		TaskID:     req.TaskID,
		Request:    &req,
		SessionDir: &sess.SessionDir,
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
	c.JSON(http.StatusOK, SessionInfo{
		SessionID:  snap.ID,
		TaskID:     snap.TaskID,
		Agent:      snap.AgentName,
		Runtime:    snap.RuntimeName,
		Status:     string(snap.State),
		CreatedAt:  snap.CreatedAt,
		EndedAt:    snap.EndedAt,
		ExitCode:   snap.ExitCode,
		SessionDir: snap.SessionDir,
		LogFile:    session.LogFilePath(s.logDir, snap.ID),
		WSURL:      sessionWSURL(c, snap.ID),
		LogURL:     sessionLogURL(c, snap.ID),
	})
}

func (s *Server) handleDeleteSession(c *gin.Context) {
	sess := s.sessions.Get(c.Param("id"))
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	_ = sess.Kill()
	sess.SetCompleted(-1)
	s.sessions.Remove(sess.ID)
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

	b := bridge.New(conn, sess.Handle, sess.Replay)
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

func (s *Server) lookupResumeSessionID(agentName, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}

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
	if sess == nil || req == nil || s.runtime == nil || s.runtime.Name() != "local" {
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

// drainTo reads from r and writes all data to w (typically a MultiWriter
// wrapping both the replay buffer and a log file).
func drainTo(sessionID, stream string, r io.ReadCloser, w io.Writer) {
	if r == nil || w == nil {
		return
	}
	buf := make([]byte, 32*1024)
	total := 0
	for {
		n, err := r.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			total += n
			if total == n {
				log.Printf("[session %s] first %s data: %d bytes", sessionID, stream, n)
			}
		}
		if err != nil {
			log.Printf("[session %s] %s closed: total=%d err=%v", sessionID, stream, total, err)
			return
		}
	}
}
