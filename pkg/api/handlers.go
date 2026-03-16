package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
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
	if req.Prompt == "" {
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

	// Build the command.
	agCfg := agent.AgentConfig{
		WorkDir: workDir,
		Env:     req.Env,
	}
	cmd, err := ag.BuildCmd(req.Prompt, agCfg)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Create the session.
	sess := session.NewSession(req.TaskID, req.Agent, s.runtime.Name(), req.Tags)
	if err := s.sessions.Add(sess); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Spawn the process.
	ctx := context.Background()
	handle, err := s.runtime.Spawn(ctx, runtime.SpawnConfig{
		SessionID: sess.ID,
		AgentName: req.Agent,
		Cmd:       cmd,
		Env:       req.Env,
		WorkDir:   workDir,
		TaskID:    req.TaskID,
		Request:   &req,
		PTY:       req.PTY,
	})
	if err != nil {
		s.sessions.Remove(sess.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sess.SetRunning(handle)

	// Watch for exit in the background.
	go func() {
		result := <-handle.Wait()
		sess.SetCompleted(result.Code)
	}()

	// Snapshot after SetRunning — the goroutine hasn't had a chance to call
	// SetCompleted yet, but we use Snapshot for correctness with the race detector.
	snap := sess.Snapshot()
	c.JSON(http.StatusCreated, SessionResponse{
		SessionID: snap.ID,
		TaskID:    snap.TaskID,
		Agent:     snap.AgentName,
		Runtime:   snap.RuntimeName,
		Status:    string(snap.State),
		WSURL:     websocketScheme(c) + "://" + c.Request.Host + "/ws/sessions/" + url.PathEscape(snap.ID),
		LogURL:    httpScheme(c) + "://" + c.Request.Host + "/sessions/" + url.PathEscape(snap.ID) + "/logs",
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

func (s *Server) handleDeleteSession(c *gin.Context) {
	sess := s.sessions.Get(c.Param("id"))
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	_ = sess.Kill()
	sess.SetCompleted(-1)
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
