// Package api provides the HTTP + WebSocket server for agentd.
package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log"
	"net/http"
	"os"
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
	router        *gin.Engine
	sessions      *session.Manager
	runtimes      map[string]runtime.Runtime // keyed by name ("local", "docker")
	runtime       runtime.Runtime            // default runtime (first registered or "local")
	agents        *agent.Registry
	version       string
	commitHash    string
	listenerScope string
	authEnabled   bool
	authTokenHash [sha256.Size]byte
	dataDir       string
	logDir        string // directory for persistent session NDJSON logs
	durableStore  durable.Store
	eventBroker   *eventstream.Broker
	observers     ObserverService
	srv           *http.Server
	resumeMu      sync.Mutex
	admissionMu   sync.RWMutex
	draining      bool
	nativeMu      sync.RWMutex
	native        map[string]*activeNativeSession
	readiness     *runtimeReadinessMonitor
	activationCtx context.Context
	activationEnd context.CancelFunc
	activationMu  sync.Mutex
	activations   int
	progress      *activationProgressBroker
	cleanupOnce   sync.Once
	cleanupWG     sync.WaitGroup
	resumeStates  *resumeStateStore

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

	// CommitHash is the exact source revision embedded in the AgentD artifact.
	CommitHash string

	// AuthToken enables bearer authentication for every private HTTP and
	// WebSocket surface. Production AgentD always supplies the token loaded
	// from its private data root; empty is reserved for isolated tests.
	AuthToken string

	// ListenerScope is the resolved bind classification advertised to callers.
	ListenerScope string

	// DataDir stores agent session state, credentials, and logs.
	// Defaults to the parent of LogDir, or "." if LogDir is also empty.
	DataDir string

	// LogDir is the directory for persistent session NDJSON log files.
	// Defaults to "./logs" if empty.
	LogDir string

	// DiagnosticLogs controls the non-canonical session NDJSON mirror. Nil
	// preserves the secure default: enabled with seven-day retention.
	DiagnosticLogs *DiagnosticLogConfig

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

	// RuntimeProbeInterval controls passive runtime readiness refreshes.
	// Zero uses the production default.
	RuntimeProbeInterval time.Duration

	// RuntimeProbeTimeout bounds one background refresh across each runtime.
	// Zero uses the production default.
	RuntimeProbeTimeout time.Duration

	// RuntimeProbeStaleAfter fails readiness when the most recent completed
	// probe is older than this duration. Zero uses the production default.
	RuntimeProbeStaleAfter time.Duration
}

// DiagnosticLogConfig controls private, redacted, non-canonical session logs.
type DiagnosticLogConfig struct {
	Enabled   bool
	Retention time.Duration
}

const DefaultDiagnosticLogRetention = 7 * 24 * time.Hour

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
	diagnosticLogs := DiagnosticLogConfig{Enabled: true, Retention: DefaultDiagnosticLogRetention}
	if len(cfgs) > 0 && cfgs[0].DiagnosticLogs != nil {
		diagnosticLogs = *cfgs[0].DiagnosticLogs
	}
	if !diagnosticLogs.Enabled {
		logDir = ""
	} else if len(cfgs) > 0 && cfgs[0].LogDir != "" {
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			log.Printf("warning: create diagnostic log directory: %v", err)
		} else if err := os.Chmod(logDir, 0o700); err != nil {
			log.Printf("warning: secure diagnostic log directory: %v", err)
		} else if err := session.SecureDiagnosticLogs(logDir); err != nil {
			log.Printf("warning: secure retained diagnostic logs: %v", err)
		} else if removed, err := session.PruneDiagnosticLogs(logDir, diagnosticLogs.Retention, time.Now()); err != nil {
			log.Printf("warning: prune diagnostic logs: %v", err)
		} else if removed > 0 {
			log.Printf("pruned %d expired diagnostic logs", removed)
		}
	}

	runtimes := map[string]runtime.Runtime{rt.Name(): rt}
	// Register extra runtimes from config.
	if len(cfgs) > 0 {
		for _, extra := range cfgs[0].ExtraRuntimes {
			runtimes[extra.Name()] = extra
		}
	}

	version := "dev"
	commitHash := "unknown"
	if len(cfgs) > 0 && cfgs[0].Version != "" {
		version = cfgs[0].Version
	}
	if len(cfgs) > 0 && cfgs[0].CommitHash != "" {
		commitHash = cfgs[0].CommitHash
	}

	activationCtx, activationEnd := context.WithCancel(context.Background())
	s := &Server{
		router:        router,
		sessions:      sessions,
		runtimes:      runtimes,
		runtime:       rt,
		agents:        agents,
		version:       version,
		commitHash:    commitHash,
		dataDir:       dataDir,
		logDir:        logDir,
		native:        make(map[string]*activeNativeSession),
		activationCtx: activationCtx,
		activationEnd: activationEnd,
		progress:      newActivationProgressBroker(),
		resumeStates:  newResumeStateStore(filepath.Join(dataDir, "resume-states")),
	}
	if len(cfgs) > 0 {
		s.listenerScope = cfgs[0].ListenerScope
		if cfgs[0].AuthToken != "" {
			s.authEnabled = true
			s.authTokenHash = sha256.Sum256([]byte(cfgs[0].AuthToken))
		}
		s.chatRegistry = cfgs[0].ChatRegistry
		s.chatManager = cfgs[0].ChatManager
		s.durableStore = cfgs[0].DurableStore
		s.eventBroker = cfgs[0].EventBroker
		s.observers = cfgs[0].ObserverService
	}
	s.readiness = newRuntimeReadinessMonitor(runtimes, configuredRuntimeReadiness(cfgs...))

	RegisterRoutes(router, s)
	return s
}

// Start begins listening on the given address. Blocks until the server is stopped.
func (s *Server) Start(addr string) error {
	s.startRuntimeReadiness()
	s.startStoppedContainerCleanup()
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Streaming and WebSocket responses own their write deadlines.
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
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
	s.stopRuntimeReadiness()

	var errs []error
	if s.srv != nil {
		if err := s.srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	s.activationEnd()
	s.waitForActivationDrain(ctx)
	s.waitForStoppedContainerCleanup(ctx)

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
