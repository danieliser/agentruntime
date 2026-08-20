// agentd is the daemon entrypoint for agentruntime. It starts an HTTP + WebSocket
// server that manages agent sessions across configured execution runtimes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/api"
	"github.com/danieliser/agentruntime/pkg/buildinfo"
	"github.com/danieliser/agentruntime/pkg/chat"
	projectconfig "github.com/danieliser/agentruntime/pkg/config"
	"github.com/danieliser/agentruntime/pkg/credentials"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/observer"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

const observerStartupSyncTimeout = 5 * time.Second

type startupObserverSyncer interface {
	Sync(context.Context) error
}

func syncObserversAtStartup(parent context.Context, observers startupObserverSyncer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return observers.Sync(ctx)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dispatch" {
		os.Exit(runDispatchCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "attach" {
		os.Exit(runAttachCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "chat" {
		os.Exit(runChatCommand(os.Args[2:]))
	}

	showVersion := flag.Bool("version", false, "Print version and exit")
	showBuildInfo := flag.Bool("build-info", false, "Print verifiable build identity as JSON and exit")
	requireBuild := flag.String("require-build", "", "Require exact version@commit identity and exit")
	host := flag.String("host", defaultListenHost(), "HTTP server bind address")
	port := flag.Int("port", 8090, "HTTP server port")
	rtName := flag.String("runtime", "local", "Execution runtime (local, docker)")
	dataDir := flag.String("data-dir", defaultDataDir(), "Data root for database, chat history, logs, credentials, and backups")
	pluginConfigPath := flag.String("plugin-config", "", "External observer allowlist (default: <data-dir>/plugins.json)")
	credSync := flag.Bool("credential-sync", false, "Enable background credential sync from Keychain")
	maxSessions := flag.Int("max-sessions", 0, "Maximum concurrent sessions (0 = unlimited)")
	dockerHost := flag.String("docker-host", "", "Remote Docker daemon (e.g., ssh://deploy@host, tcp://host:2376)")
	diagnosticLogs := flag.Bool("diagnostic-logs", true, "Write private redacted session diagnostic logs")
	diagnosticLogRetention := flag.Duration("diagnostic-log-retention", api.DefaultDiagnosticLogRetention, "Diagnostic log retention (0 keeps indefinitely)")
	flag.Parse()

	identity := buildinfo.Current()
	if *showVersion {
		fmt.Println(identity.Version)
		os.Exit(0)
	}
	if *showBuildInfo {
		if err := json.NewEncoder(os.Stdout).Encode(identity); err != nil {
			log.Fatalf("encode build identity: %v", err)
		}
		os.Exit(0)
	}
	if *requireBuild != "" {
		if err := identity.Verify(*requireBuild); err != nil {
			log.Fatalf("build qualification failed: %v", err)
		}
		fmt.Printf("verified agentd %s@%s\n", identity.Version, identity.Commit)
		os.Exit(0)
	}
	diagnosticLogConfig, err := configuredDiagnosticLogs(*diagnosticLogs, *diagnosticLogRetention)
	if err != nil {
		log.Fatalf("invalid diagnostic log configuration: %v", err)
	}

	log.Printf("agentd %s (%s) starting", identity.Version, identity.Commit)
	log.Printf("data dir: %s", *dataDir)
	authToken, err := api.LoadOrCreateAuthToken(*dataDir)
	if err != nil {
		log.Fatalf("failed to initialize API authentication: %v", err)
	}
	listenerScope, err := classifyListenHost(*host)
	if err != nil {
		log.Fatalf("invalid HTTP bind address: %v", err)
	}
	logDir := filepath.Join(*dataDir, "logs")
	durableStore, err := openDurableStore(*dataDir)
	if err != nil {
		log.Fatalf("failed to initialize durable store: %v", err)
	}
	defer func() {
		if err := durableStore.Close(); err != nil {
			log.Printf("durable store close error: %v", err)
		}
	}()
	if err := durableStore.CheckIntegrity(context.Background()); err != nil {
		log.Fatalf("durable store integrity check failed: %v", err)
	}
	eventBroker := eventstream.New(durableStore)
	resolvedPluginConfig := *pluginConfigPath
	if resolvedPluginConfig == "" {
		resolvedPluginConfig = defaultPluginConfigPath(*dataDir)
	}
	pluginConfig, err := observer.LoadOptionalConfig(resolvedPluginConfig)
	if err != nil {
		log.Fatalf("failed to load observer config: %v", err)
	}
	traceObservers, err := observer.NewManager(*dataDir, durableStore, pluginConfig, identity.Version, eventstream.SchemaVersion)
	if err != nil {
		log.Fatalf("failed to initialize observers: %v", err)
	}
	eventBroker.SetCommittedObserver(traceObservers.Notify)
	// A best-effort observer backlog must not indefinitely delay the runtime
	// API. The background worker resumes from its durable checkpoint afterward.
	if err := syncObserversAtStartup(context.Background(), traceObservers, observerStartupSyncTimeout); err != nil {
		log.Printf("observer startup degraded: %v", err)
	}
	traceObservers.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := traceObservers.Close(ctx); err != nil {
			log.Printf("observer shutdown error: %v", err)
		}
	}()

	// Initialize runtimes. The --runtime flag sets the default; both local
	// and docker are always available so callers can select per-session.
	rt, err := newRuntime(*rtName, *dataDir, *dockerHost, identity, diagnosticLogConfig.Enabled)
	if err != nil {
		log.Fatalf("failed to initialize runtime: %v", err)
	}
	var extraRuntimes []runtime.Runtime
	if *rtName != "local" {
		extraRuntimes = append(extraRuntimes, runtime.NewLocalRuntime())
	}
	if *rtName != "docker" {
		// Docker runtime init is lazy: if Docker isn't available, log a warning
		// but don't fail startup. The runtime will return an error on Spawn().
		dockerRT := runtime.NewDockerRuntime(dockerConfigForBuild(*dataDir, *dockerHost, identity, diagnosticLogConfig.Enabled))
		extraRuntimes = append(extraRuntimes, dockerRT)
	}

	// Initialize session manager and recover orphaned sessions.
	sessions := session.NewManager()
	if *maxSessions > 0 {
		sessions.SetMaxSessions(*maxSessions)
		log.Printf("max sessions: %d", *maxSessions)
	}

	// Recover from primary runtime and all extra runtimes.
	allRuntimes := []runtime.Runtime{rt}
	allRuntimes = append(allRuntimes, extraRuntimes...)

	totalRecovered := 0
	var recoveredSessions []*session.Session
	var recoveredRuntimes []string
	for _, r := range allRuntimes {
		recovered, err := r.Recover(context.Background())
		if err != nil {
			log.Printf("warning: %s runtime recovery failed: %v", r.Name(), err)
			continue
		}
		recoveredRuntimes = append(recoveredRuntimes, r.Name())
		if len(recovered) > 0 {
			orphaned := sessions.Recover(recovered, r.Name())
			recoveredSessions = append(recoveredSessions, orphaned...)
			totalRecovered += len(orphaned)
		}
	}
	if totalRecovered > 0 {
		log.Printf("recovered %d orphaned sessions from all runtimes", totalRecovered)
	}

	// Initialize chat subsystem (named persistent chats with idle timeout).
	chatRegistry, err := chat.NewRegistry(*dataDir)
	if err != nil {
		log.Fatalf("failed to initialize chat registry: %v", err)
	}

	runtimeMap := make(map[string]runtime.Runtime, len(allRuntimes))
	for _, r := range allRuntimes {
		runtimeMap[r.Name()] = r
	}
	// Wire the Docker runtime as the chat VolumeManager so chat volumes
	// are created for Docker-backed chats. Falls back to nil (no volumes)
	// when Docker isn't available.
	var chatVolumes chat.VolumeManager
	if dockerRT, ok := runtimeMap["docker"].(*runtime.DockerRuntime); ok {
		chatVolumes = &dockerVolumeAdapter{rt: dockerRT}
	}
	chatManager := chat.NewManager(chatRegistry, sessions, runtimeMap, *rtName, chatVolumes, nil)
	chatWatcher := chat.NewIdleWatcher(chatRegistry, sessions, chatManager)

	chatCtx, chatCancel := context.WithCancel(context.Background())
	defer chatCancel()
	chatWatcher.Start(chatCtx)

	// Recover running chats: any chat with state=="running" whose session
	// no longer exists should be transitioned to idle.
	recoverRunningChats(chatRegistry, sessions)

	// Optional credential sync.
	if *credSync {
		creds := credentials.NewSync(*dataDir)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		creds.Watch(ctx, 30*time.Second)
		creds.CodexWatch(ctx, 12*time.Hour)
		log.Println("credential sync enabled (Claude: 30s, Codex: 12h)")
	}

	// Initialize agent registry.
	agents := agent.DefaultRegistry()

	// Start HTTP server.
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	srv := api.NewServer(sessions, rt, agents, api.ServerConfig{
		Version:         identity.Version,
		CommitHash:      identity.Commit,
		AuthToken:       authToken,
		ListenerScope:   listenerScope,
		DataDir:         *dataDir,
		LogDir:          logDir,
		DiagnosticLogs:  &diagnosticLogConfig,
		ExtraRuntimes:   extraRuntimes,
		ChatRegistry:    chatRegistry,
		ChatManager:     chatManager,
		DurableStore:    durableStore,
		EventBroker:     eventBroker,
		ObserverService: traceObservers,
	})
	srv.RestoreRecoveredSessions(recoveredSessions, recoveredRuntimes...)
	// Wire the spawner after server creation to break the circular dependency
	// between api.Server (needs chatManager) and chatManager (needs spawner).
	chatManager.SetSpawner(srv)

	// Graceful shutdown on SIGINT/SIGTERM.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("received %v, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5e9) // 5s
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	if err := srv.Start(addr); err != nil {
		// http.ErrServerClosed is expected on graceful shutdown.
		if err.Error() != "http: Server closed" {
			log.Fatalf("server error: %v", err)
		}
	}
	log.Println("agentd stopped")
}

func configuredDiagnosticLogs(enabled bool, retention time.Duration) (api.DiagnosticLogConfig, error) {
	if raw := os.Getenv("AGENTD_DIAGNOSTIC_LOGS"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return api.DiagnosticLogConfig{}, fmt.Errorf("AGENTD_DIAGNOSTIC_LOGS must be a boolean")
		}
		enabled = parsed
	}
	if raw := os.Getenv("AGENTD_DIAGNOSTIC_LOG_RETENTION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return api.DiagnosticLogConfig{}, fmt.Errorf("AGENTD_DIAGNOSTIC_LOG_RETENTION must be a duration")
		}
		retention = parsed
	}
	if retention < 0 {
		return api.DiagnosticLogConfig{}, fmt.Errorf("diagnostic log retention must be nonnegative")
	}
	return api.DiagnosticLogConfig{Enabled: enabled, Retention: retention}, nil
}

func defaultListenHost() string { return "127.0.0.1" }

func classifyListenHost(host string) (string, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("host must be a literal IP address")
	}
	switch {
	case ip.IsLoopback():
		return "loopback", nil
	case ip.IsUnspecified():
		return "all_interfaces", nil
	case ip.IsPrivate():
		return "private", nil
	default:
		return "public", nil
	}
}

// defaultDataDir returns AgentD's private user-state root. An explicit
// AGENTRUNTIME_DATA_DIR relocates the complete database/history/log layout.
func defaultDataDir() string {
	return projectconfig.DataDir()
}

func defaultPluginConfigPath(dataDir string) string {
	if path := os.Getenv("AGENTRUNTIME_PLUGIN_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(dataDir, "plugins.json")
}

func openDurableStore(dataDir string) (*durablesqlite.Store, error) {
	return durablesqlite.Open(filepath.Join(dataDir, "agentd.sqlite"))
}

// recoverRunningChats transitions any chat with state=="running" whose session
// is no longer in the session manager to idle. This handles daemon restarts
// where the agent process was lost.
func recoverRunningChats(reg *chat.Registry, sm *session.Manager) {
	chats, err := reg.List()
	if err != nil {
		log.Printf("warning: failed to list chats for recovery: %v", err)
		return
	}
	for _, c := range chats {
		if c.State != chat.ChatStateRunning {
			continue
		}
		if sm.Get(c.CurrentSession) != nil {
			continue
		}
		oldSession := c.CurrentSession
		c.State = chat.ChatStateIdle
		c.CurrentSession = ""
		if err := reg.Save(c); err != nil {
			log.Printf("warning: failed to save recovered chat %q: %v", c.Name, err)
			continue
		}
		log.Printf("chat %q recovered to idle (session %s not found)", c.Name, oldSession)
	}
}

// dockerVolumeAdapter adapts DockerRuntime to the chat.VolumeManager interface.
type dockerVolumeAdapter struct {
	rt *runtime.DockerRuntime
}

func (a *dockerVolumeAdapter) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return a.rt.CreateNamedVolume(ctx, name, labels)
}

func (a *dockerVolumeAdapter) RemoveVolume(ctx context.Context, name string) error {
	return a.rt.RemoveSessionVolume(ctx, name)
}

func newRuntime(name, dataDir, dockerHost string, identity buildinfo.Identity, diagnosticLogs ...bool) (runtime.Runtime, error) {
	switch name {
	case "local":
		return runtime.NewLocalRuntime(), nil
	case "docker":
		return runtime.NewDockerRuntime(dockerConfigForBuild(dataDir, dockerHost, identity, diagnosticLogs...)), nil
	default:
		return nil, fmt.Errorf("unknown runtime: %s", name)
	}
}

func dockerConfigForBuild(dataDir, dockerHost string, identity buildinfo.Identity, diagnosticLogs ...bool) runtime.DockerConfig {
	cfg := runtime.DockerConfig{DataDir: dataDir, Host: dockerHost}
	diagnosticsEnabled := true
	if len(diagnosticLogs) > 0 {
		diagnosticsEnabled = diagnosticLogs[0]
	}
	if diagnosticsEnabled {
		cfg.DiagnosticDir = filepath.Join(dataDir, "logs")
	}
	if identity.Version == "" || identity.Version == "dev" || identity.Commit == "" || identity.Commit == "unknown" {
		cfg.Image = runtime.DefaultDockerImage
		cfg.ProxyImage = "agentruntime-proxy:latest"
		return cfg
	}
	cfg.Image = "agentruntime-agent:" + identity.Version
	cfg.ProxyImage = "agentruntime-proxy:" + identity.Version
	cfg.ExpectedVersion = identity.Version
	cfg.ExpectedCommit = identity.Commit
	return cfg
}
