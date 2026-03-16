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
	"syscall"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/api"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func main() {
	port := flag.Int("port", 8090, "HTTP server port")
	rtName := flag.String("runtime", "local", "Execution runtime (local, docker, opensandbox)")
	flag.Parse()

	// Initialize runtime.
	rt, err := newRuntime(*rtName)
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
		log.Printf("recovered %d orphaned sessions", len(orphaned))
	}

	// Initialize agent registry.
	agents := agent.DefaultRegistry()

	// Start HTTP server.
	addr := fmt.Sprintf(":%d", *port)
	srv := api.NewServer(sessions, rt, agents)

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

func newRuntime(name string) (runtime.Runtime, error) {
	switch name {
	case "local":
		return runtime.NewLocalRuntime(), nil
	case "docker":
		return runtime.NewDockerRuntime(runtime.DockerConfig{
			Image: "alpine:latest",
		}), nil
	case "opensandbox":
		return &runtime.OpenSandboxRuntime{}, nil
	default:
		return nil, fmt.Errorf("unknown runtime: %s", name)
	}
}
