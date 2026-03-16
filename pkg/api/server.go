// Package api provides the HTTP + WebSocket server for agentd.
package api

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// Server holds the HTTP server and its dependencies.
type Server struct {
	router   *gin.Engine
	sessions *session.Manager
	runtime  runtime.Runtime
	agents   *agent.Registry
	dataDir  string
	logDir   string // directory for persistent session NDJSON logs
	srv      *http.Server
}

// ServerConfig holds optional configuration for the server.
type ServerConfig struct {
	// DataDir stores agent session state, credentials, and logs.
	// Defaults to the parent of LogDir, or "." if LogDir is also empty.
	DataDir string

	// LogDir is the directory for persistent session NDJSON log files.
	// Defaults to "./logs" if empty.
	LogDir string
}

// NewServer creates a configured HTTP server ready to start.
func NewServer(sessions *session.Manager, rt runtime.Runtime, agents *agent.Registry, cfgs ...ServerConfig) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	logDir := "./logs"
	if len(cfgs) > 0 && cfgs[0].LogDir != "" {
		logDir = cfgs[0].LogDir
	}
	dataDir := filepath.Dir(logDir)
	if len(cfgs) > 0 && cfgs[0].DataDir != "" {
		dataDir = cfgs[0].DataDir
	}

	s := &Server{
		router:   router,
		sessions: sessions,
		runtime:  rt,
		agents:   agents,
		dataDir:  dataDir,
		logDir:   logDir,
	}

	RegisterRoutes(router, s)
	return s
}

// Start begins listening on the given address. Blocks until the server is stopped.
func (s *Server) Start(addr string) error {
	s.srv = &http.Server{
		Addr:         addr,
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	log.Printf("agentd listening on %s (runtime: %s)", addr, s.runtime.Name())
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}
