// Package api provides the HTTP + WebSocket server for agentd.
package api

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/chat"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// Server holds the HTTP server and its dependencies.
type Server struct {
	router       *gin.Engine
	sessions     *session.Manager
	runtimes     map[string]runtime.Runtime // keyed by name ("local", "docker")
	runtime      runtime.Runtime            // default runtime (first registered or "local")
	agents       *agent.Registry
	version      string
	dataDir      string
	logDir       string // directory for persistent session NDJSON logs
	durableStore durable.Store
	eventBroker  *eventstream.Broker
	srv          *http.Server
	resumeMu     sync.Mutex
	nativeMu     sync.RWMutex
	native       map[string]nativeprotocol.Transport

	// Chat subsystem (named persistent chats).
	chatRegistry *chat.Registry
	chatManager  *chat.Manager
}

// RuntimeFor returns the runtime matching the requested name, or the default.
func (s *Server) RuntimeFor(name string) runtime.Runtime {
	if name == "" {
		return s.runtime
	}
	if rt, ok := s.runtimes[name]; ok {
		return rt
	}
	return nil
}

// ServerConfig holds optional configuration for the server.
type ServerConfig struct {
	// Version is the agentd build version string (e.g., "0.7.1").
	Version string

	// DataDir stores agent session state, credentials, and logs.
	// Defaults to the parent of LogDir, or "." if LogDir is also empty.
	DataDir string

	// LogDir is the directory for persistent session NDJSON log files.
	// Defaults to "./logs" if empty.
	LogDir string

	// ExtraRuntimes are additional runtimes beyond the primary one.
	// Each is registered by its Name() and selectable via req.Runtime.
	ExtraRuntimes []runtime.Runtime

	// ChatRegistry is the file-based chat record store. Optional.
	ChatRegistry *chat.Registry

	// ChatManager orchestrates named chat lifecycle. Optional.
	ChatManager *chat.Manager

	// DurableStore owns reconstructable sessions, generations, event history,
	// and terminal receipts. Current compatibility handlers are migrated to it
	// incrementally rather than treating legacy logs as proven durable events.
	DurableStore durable.Store

	// EventBroker commits native events before live publication and owns the
	// stored-to-live subscription handshake.
	EventBroker *eventstream.Broker
}

// NewServer creates a configured HTTP server ready to start.
// Accepts one or more runtimes. The first runtime is the default.
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

	runtimes := map[string]runtime.Runtime{rt.Name(): rt}
	// Register extra runtimes from config.
	if len(cfgs) > 0 {
		for _, extra := range cfgs[0].ExtraRuntimes {
			runtimes[extra.Name()] = extra
		}
	}

	version := "dev"
	if len(cfgs) > 0 && cfgs[0].Version != "" {
		version = cfgs[0].Version
	}

	s := &Server{
		router:   router,
		sessions: sessions,
		runtimes: runtimes,
		runtime:  rt,
		agents:   agents,
		version:  version,
		dataDir:  dataDir,
		logDir:   logDir,
		native:   make(map[string]nativeprotocol.Transport),
	}
	if len(cfgs) > 0 {
		s.chatRegistry = cfgs[0].ChatRegistry
		s.chatManager = cfgs[0].ChatManager
		s.durableStore = cfgs[0].DurableStore
		s.eventBroker = cfgs[0].EventBroker
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
	names := make([]string, 0, len(s.runtimes))
	for name := range s.runtimes {
		names = append(names, name)
	}
	log.Printf("agentd listening on %s (runtimes: %v, default: %s)", addr, names, s.runtime.Name())
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the server, killing all active sessions first,
// then cleaning up all runtime infrastructure.
func (s *Server) Shutdown(ctx context.Context) error {
	s.sessions.ShutdownAll()

	// Shut down HTTP server.
	var errs []error
	if s.srv != nil {
		if err := s.srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	// Clean up all runtimes.
	for _, r := range s.runtimes {
		if err := r.Cleanup(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errs[0] // Return first error; caller can use errors.Join if needed
	}
	return nil
}
