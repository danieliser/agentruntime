// Package api provides the HTTP + WebSocket server for agentd.
package api

import (
	"context"
	"errors"
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
	"github.com/danieliser/agentruntime/pkg/observer"
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
	observers    ObserverService
	srv          *http.Server
	resumeMu     sync.Mutex
	admissionMu  sync.RWMutex
	draining     bool
	nativeMu     sync.RWMutex
	native       map[string]*activeNativeSession

	// Chat subsystem (named persistent chats).
	chatRegistry *chat.Registry
	chatManager  *chat.Manager
}

var errAdmissionClosed = errors.New("agentd is draining and no longer accepts new work")

func (s *Server) beginAdmission() bool {
	s.admissionMu.RLock()
	if s.draining {
		s.admissionMu.RUnlock()
		return false
	}
	return true
}

func (s *Server) endAdmission() {
	s.admissionMu.RUnlock()
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
	// and terminal receipts. API handlers use it
	// incrementally rather than treating legacy logs as proven durable events.
	DurableStore durable.Store

	// EventBroker commits native events before live publication and owns the
	// stored-to-live subscription handshake.
	EventBroker *eventstream.Broker

	// ObserverService exposes independently supervised immutable-event plugins.
	ObserverService ObserverService
}

type ObserverService interface {
	Status() []observer.PluginStatus
	TraceLink(pluginName, sessionID string) (observer.TraceLink, bool)
	RequireHealthy(pluginName string) error
	Policy(pluginName string) (observer.Policy, bool)
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
		native:   make(map[string]*activeNativeSession),
	}
	if len(cfgs) > 0 {
		s.chatRegistry = cfgs[0].ChatRegistry
		s.chatManager = cfgs[0].ChatManager
		s.durableStore = cfgs[0].DurableStore
		s.eventBroker = cfgs[0].EventBroker
		s.observers = cfgs[0].ObserverService
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

// Shutdown closes admission, allows active work to drain until ctx expires,
// preserves live Docker generations for restart reconstruction, and only
// tears down runtime infrastructure that has no preserved work.
func (s *Server) Shutdown(ctx context.Context) error {
	s.admissionMu.Lock()
	s.draining = true
	s.admissionMu.Unlock()

	var errs []error
	if s.srv != nil {
		if err := s.srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	waitForSessionDrain(ctx, s.sessions)
	preserveDocker := false
	for _, sess := range s.sessions.List() {
		snapshot := sess.Snapshot()
		active := snapshot.State != session.StateCompleted && snapshot.State != session.StateFailed
		if snapshot.RuntimeName == "docker" && active {
			preserveDocker = true
			continue
		}
		if active {
			_ = sess.Kill()
			sess.SetCompleted(-1)
		}
		sess.Replay.Close()
		s.sessions.Remove(sess.ID)
	}

	cleanupCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		cleanupCtx, cancel = context.WithTimeout(context.Background(), time.Second)
		defer cancel()
	}
	for _, r := range s.runtimes {
		if preserveDocker && r.Name() == "docker" {
			continue
		}
		if err := r.Cleanup(cleanupCtx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func waitForSessionDrain(ctx context.Context, manager *session.Manager) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		active := false
		for _, sess := range manager.List() {
			state := sess.Snapshot().State
			if state != session.StateCompleted && state != session.StateFailed {
				active = true
				break
			}
		}
		if !active {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
