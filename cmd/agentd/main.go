// agentd is the daemon entrypoint for agentruntime. It starts an HTTP + WebSocket
// server that manages agent sessions across configured execution runtimes.
package main

import (
	"context"
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
	"github.com/danieliser/agentruntime/pkg/credentials"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dispatch" {
		os.Exit(runDispatchCommand(os.Args[2:]))
	}

	port := flag.Int("port", 8090, "HTTP server port")
	rtName := flag.String("runtime", "local", "Execution runtime (local, docker, opensandbox)")
	dataDir := flag.String("data-dir", defaultDataDir(), "Data directory for sessions, logs, credentials")
	credSync := flag.Bool("credential-sync", false, "Enable background credential sync from Keychain")
	flag.Parse()

	log.Printf("data dir: %s", *dataDir)
	logDir := filepath.Join(*dataDir, "logs")

	// Initialize runtime.
	rt, err := newRuntime(*rtName, *dataDir)
	if err != nil {
		log.Fatalf("failed to initialize runtime: %v", err)
	}

	// Initialize session manager and recover orphaned sessions.
	sessions := session.NewManager()
	recovered, err := rt.Recover(context.Background())
	if err != nil {
		log.Printf("warning: runtime recovery failed: %v", err)
	}
	if len(recovered) > 0 {
		orphaned := sessions.Recover(recovered, rt.Name())
		restoreRecoveredSessions(logDir, orphaned)
		log.Printf("recovered %d orphaned sessions", len(orphaned))
	}

	// Optional credential sync.
	if *credSync {
		creds := credentials.NewSync(*dataDir)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		creds.Watch(ctx, 30*time.Second)
		log.Println("credential sync enabled (30s interval)")
	}

	// Initialize agent registry.
	agents := agent.DefaultRegistry()

	// Start HTTP server.
	addr := fmt.Sprintf(":%d", *port)
	srv := api.NewServer(sessions, rt, agents, api.ServerConfig{
		DataDir: *dataDir,
		LogDir:  logDir,
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

func newRuntime(name, dataDir string) (runtime.Runtime, error) {
	switch name {
	case "local":
		return runtime.NewLocalRuntime(), nil
	case "docker":
		return runtime.NewDockerRuntime(runtime.DockerConfig{
			DataDir: dataDir,
		}), nil
	case "opensandbox":
		return &runtime.OpenSandboxRuntime{}, nil
	default:
		return nil, fmt.Errorf("unknown runtime: %s", name)
	}
}
