// agentd is the daemon entrypoint for agentruntime. It starts an HTTP + WebSocket
// server that manages agent sessions across configured execution runtimes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/api"
	"github.com/danieliser/agentruntime/pkg/chat"
	"github.com/danieliser/agentruntime/pkg/credentials"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

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

	port := flag.Int("port", 8090, "HTTP server port")
	rtName := flag.String("runtime", "local", "Execution runtime (local, docker)")
	dataDir := flag.String("data-dir", defaultDataDir(), "Data directory for sessions, logs, credentials")
	credSync := flag.Bool("credential-sync", false, "Enable background credential sync from Keychain")
	maxSessions := flag.Int("max-sessions", 0, "Maximum concurrent sessions (0 = unlimited)")
	dockerHost := flag.String("docker-host", "", "Remote Docker daemon (e.g., ssh://deploy@host, tcp://host:2376)")
	flag.Parse()

	log.Printf("data dir: %s", *dataDir)
	logDir := filepath.Join(*dataDir, "logs")

	// Initialize runtimes. The --runtime flag sets the default; both local
	// and docker are always available so callers can select per-session.
	rt, err := newRuntime(*rtName, *dataDir, *dockerHost)
	if err != nil {
		log.Fatalf("failed to initialize runtime: %v", err)
	}
	var extraRuntimes []runtime.Runtime
	if *rtName != "local" {
		extraRuntimes = append(extraRuntimes, runtime.NewLocalSidecarRuntime())
	}
	if *rtName != "docker" {
		// Docker runtime init is lazy: if Docker isn't available, log a warning
		// but don't fail startup. The runtime will return an error on Spawn().
		dockerRT := runtime.NewDockerRuntime(runtime.DockerConfig{
			DataDir: *dataDir,
			Host:    *dockerHost,
		})
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
	for _, r := range allRuntimes {
		recovered, err := r.Recover(context.Background())
		if err != nil {
			log.Printf("warning: %s runtime recovery failed: %v", r.Name(), err)
			continue
		}
		if len(recovered) > 0 {
			orphaned := sessions.Recover(recovered, r.Name())
			restoreRecoveredSessions(logDir, orphaned)
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
	chatManager := chat.NewManager(chatRegistry, sessions, runtimeMap, *rtName, nil, nil)
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
	addr := fmt.Sprintf(":%d", *port)
	srv := api.NewServer(sessions, rt, agents, api.ServerConfig{
		DataDir:       *dataDir,
		LogDir:        logDir,
		ExtraRuntimes: extraRuntimes,
		ChatRegistry:  chatRegistry,
		ChatManager:   chatManager,
	})

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

		// Tear down runtime infrastructure (proxy containers, networks) from all runtimes.
		var cleanupErrs []error
		allRuntimes := []runtime.Runtime{rt}
		allRuntimes = append(allRuntimes, extraRuntimes...)
		for _, r := range allRuntimes {
			if err := r.Cleanup(ctx); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("%s runtime cleanup: %w", r.Name(), err))
			}
		}
		if len(cleanupErrs) > 0 {
			log.Printf("runtime cleanup errors: %v", errors.Join(cleanupErrs...))
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

func restoreRecoveredSessions(logDir string, sessions []*session.Session) {
	for _, sess := range sessions {
		var restoredBytes int64
		logPath, exists, err := session.ExistingLogFilePath(logDir, sess.ID)
		if err != nil {
			log.Printf("[session %s] warning: check replay log failed: %v", sess.ID, err)
		} else if exists {
			if err := sess.Replay.LoadFromFile(logPath); err != nil {
				log.Printf("[session %s] warning: restore replay from %s failed: %v", sess.ID, logPath, err)
			} else {
				restoredBytes = sess.Replay.TotalBytes()
			}
		}

		api.AttachSessionIO(sess, logDir)
		log.Printf("recovered session %s: replay loaded (%d bytes), stdio reattached", sess.ID, restoredBytes)
	}
}

// defaultDataDir returns the XDG-compliant data directory.
// Respects AGENTRUNTIME_DATA_DIR env, then XDG_DATA_HOME, then ~/.local/share/agentruntime.
func defaultDataDir() string {
	if dir := os.Getenv("AGENTRUNTIME_DATA_DIR"); dir != "" {
		return dir
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "agentruntime")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "agentruntime")
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

func newRuntime(name, dataDir, dockerHost string) (runtime.Runtime, error) {
	switch name {
	case "local":
		return runtime.NewLocalSidecarRuntime(), nil
	case "local-pipe":
		// Legacy pipe-based local runtime (no sidecar, no structured events)
		return runtime.NewLocalRuntime(), nil
	case "docker":
		return runtime.NewDockerRuntime(runtime.DockerConfig{
			DataDir: dataDir,
			Host:    dockerHost,
		}), nil
	default:
		return nil, fmt.Errorf("unknown runtime: %s", name)
	}
}
