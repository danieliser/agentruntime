package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const defaultPort = "9090"

type sidecarServer interface {
	AgentType() string
	Routes() http.Handler
	Close() error
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	server, port, err := newSidecarFromEnv()
	if err != nil {
		return err
	}

	log.Printf("sidecar listening on :%s (agent=%s)", port, server.AgentType())

	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: server.Routes(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = server.Close()
			return err
		}
		return server.Close()
	case <-signalCtx.Done():
		log.Printf("signal received, interrupting agent and shutting down")
		if err := interruptServer(server); err != nil {
			log.Printf("interrupt agent: %v", err)
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		shutdownErr := httpServer.Shutdown(shutdownCtx)
		closeErr := server.Close()
		err := <-errCh
		if shutdownErr != nil {
			return shutdownErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func newSidecarFromEnv() (sidecarServer, string, error) {
	port := os.Getenv("SIDECAR_PORT")
	if port == "" {
		port = defaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, "", errors.New("SIDECAR_PORT must be numeric")
	}

	if raw := os.Getenv("AGENT_CMD"); raw != "" {
		cmd, err := parseAgentCommand(raw)
		if err != nil {
			return nil, "", err
		}

		agentType := detectAgentType(cmd)
		backend, err := newBackend(agentType, cmd)
		if err != nil {
			return nil, "", err
		}
		return NewExternalWSServer(agentType, backend), port, nil
	}

	cmd, ok, err := legacyCommandFromEnv()
	if err != nil {
		return nil, "", err
	}
	if ok {
		return newLegacyPTYSidecar(cmd), port, nil
	}

	return nil, "", errors.New("AGENT_CMD is required")
}

func parseAgentCommand(raw string) ([]string, error) {
	if raw == "" {
		return nil, errors.New("AGENT_CMD is required")
	}

	var cmd []string
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		return nil, err
	}
	if len(cmd) == 0 || cmd[0] == "" {
		return nil, errors.New("AGENT_CMD must contain a command")
	}
	return cmd, nil
}

func legacyCommandFromEnv() ([]string, bool, error) {
	binary := strings.TrimSpace(firstNonEmptyEnv("AGENT_BIN", "AGENT_BINARY", "AGENT_COMMAND"))
	if binary == "" {
		return nil, false, nil
	}

	args, err := parseLegacyArgs(os.Getenv("AGENT_ARGS_JSON"), os.Getenv("AGENT_ARGS"))
	if err != nil {
		return nil, false, err
	}

	cmd := append([]string{binary}, args...)
	return cmd, true, nil
}

func parseLegacyArgs(rawJSON, raw string) ([]string, error) {
	if rawJSON != "" {
		var args []string
		if err := json.Unmarshal([]byte(rawJSON), &args); err != nil {
			return nil, err
		}
		return args, nil
	}
	if raw == "" {
		return nil, nil
	}

	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err == nil {
		return args, nil
	}
	return strings.Fields(raw), nil
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func detectAgentType(cmd []string) string {
	if len(cmd) == 0 {
		return "unknown"
	}

	name := strings.ToLower(filepath.Base(cmd[0]))
	switch {
	case strings.Contains(name, "claude"):
		return "claude"
	case strings.Contains(name, "codex"):
		return "codex"
	default:
		return name
	}
}

func interruptServer(server sidecarServer) error {
	type interrupter interface {
		Interrupt() error
	}

	if server == nil {
		return nil
	}
	if s, ok := server.(interrupter); ok {
		return s.Interrupt()
	}
	return nil
}

func newBackend(agentType string, cmd []string) (AgentBackend, error) {
	// AGENT_PROMPT triggers fire-and-forget (-p) mode.
	// If empty, the sidecar runs in interactive mode (default).
	prompt := os.Getenv("AGENT_PROMPT")

	switch agentType {
	case "claude":
		return NewClaudeBackend(ClaudeBackendConfig{
			Binary: cmd[0],
			Prompt: prompt,
		}), nil
	case "codex":
		if prompt != "" {
			return newCodexBackendPromptMode(cmd[0], prompt), nil
		}
		return newCodexBackendWithBinary(cmd[0]), nil
	default:
		return newUnsupportedBackend(agentType), nil
	}
}

type unsupportedBackend struct {
	agentType string
	sessionID string
	events    chan Event
	waitCh    chan int
}

func newUnsupportedBackend(agentType string) *unsupportedBackend {
	return &unsupportedBackend{
		agentType: agentType,
		sessionID: uuid.NewString(),
		events:    make(chan Event),
		waitCh:    make(chan int),
	}
}

func (b *unsupportedBackend) Start(context.Context) error { return nil }

func (b *unsupportedBackend) SendPrompt(string) error {
	return errors.New("prompt routing is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendInterrupt() error {
	return errors.New("interrupt routing is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendSteer(string) error {
	return errors.New("steering is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendContext(string, string) error {
	return errors.New("context injection is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendMention(string, int, int) error {
	return errors.New("mentions are not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) Events() <-chan Event { return b.events }
func (b *unsupportedBackend) SessionID() string    { return b.sessionID }
func (b *unsupportedBackend) Running() bool        { return false }
func (b *unsupportedBackend) Wait() <-chan int     { return b.waitCh }
func (b *unsupportedBackend) Close() error         { return nil }
