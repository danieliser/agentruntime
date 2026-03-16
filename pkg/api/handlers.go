package api

import (
	"context"
	"fmt"
	"net/http"
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

// CreateSessionRequest is the JSON body for POST /sessions.
type CreateSessionRequest struct {
	Agent   string            `json:"agent" binding:"required"`
	Prompt  string            `json:"prompt" binding:"required"`
	TaskID  string            `json:"task_id,omitempty"`
	WorkDir string            `json:"work_dir,omitempty"`
	Model   string            `json:"model,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"runtime": s.runtime.Name(),
	})
}

func (s *Server) handleCreateSession(c *gin.Context) {
	var req CreateSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Look up the agent.
	ag := s.agents.Get(req.Agent)
	if ag == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown agent: %s", req.Agent)})
		return
	}

	// Build the command.
	agCfg := agent.AgentConfig{
		Model:   req.Model,
		WorkDir: req.WorkDir,
		Env:     req.Env,
	}
	cmd, err := ag.BuildCmd(req.Prompt, agCfg)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Create the session.
	sess := session.NewSession(req.TaskID, req.Agent, s.runtime.Name())
	if err := s.sessions.Add(sess); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Spawn the process.
	ctx := context.Background()
	handle, err := s.runtime.Spawn(ctx, runtime.SpawnConfig{
		AgentName: req.Agent,
		Cmd:       cmd,
		Env:       req.Env,
		WorkDir:   req.WorkDir,
		TaskID:    req.TaskID,
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
	c.JSON(http.StatusCreated, gin.H{
		"id":      snap.ID,
		"state":   snap.State,
		"agent":   snap.AgentName,
		"runtime": snap.RuntimeName,
	})
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

