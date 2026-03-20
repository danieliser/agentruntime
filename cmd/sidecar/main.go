package main

import (
	"context"
	"encoding/base64"
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

)

const defaultPort = "9090"
const defaultCleanupTimeout = 60 * time.Second

type sidecarServer interface {
	AgentType() string
	Routes() http.Handler
	Close() error
}

type cleanupTimeoutConfigurer interface {
	SetCleanupTimeout(time.Duration)
}

type shutdownConfigurer interface {
	SetShutdownFunc(func())
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

	cleanupCh := make(chan struct{}, 1)
	if configurable, ok := server.(shutdownConfigurer); ok {
		configurable.SetShutdownFunc(func() {
			select {
			case cleanupCh <- struct{}{}:
			default:
			}
		})
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
	case <-cleanupCh:
		log.Printf("cleanup timeout elapsed, shutting down sidecar")
		return shutdownSidecar(httpServer, server, errCh)
	case <-signalCtx.Done():
		log.Printf("signal received, interrupting agent and shutting down")
		if err := interruptServer(server); err != nil {
			log.Printf("interrupt agent: %v", err)
		}
		return shutdownSidecar(httpServer, server, errCh)
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

		agentCfg, err := parseAgentConfig()
		if err != nil {
			return nil, "", err
		}

		agentType := detectAgentType(cmd)
		backend, err := newBackend(agentType, cmd, agentCfg)
		if err != nil {
			return nil, "", err
		}

		stallCfg := stallConfigFromAgentConfig(agentCfg)
		// Interactive mode (no AGENT_PROMPT): disable result grace period —
		// the agent sends "result" after each turn but stays alive for more.
		if os.Getenv("AGENT_PROMPT") == "" {
			stallCfg.ResultGrace = -1
		}
		server := NewExternalWSServer(agentType, backend, stallCfg)
		if err := configureCleanupTimeout(server); err != nil {
			return nil, "", err
		}
		return server, port, nil
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

func shutdownSidecar(httpServer *http.Server, server sidecarServer, errCh <-chan error) error {
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

func configureCleanupTimeout(server sidecarServer) error {
	configurable, ok := server.(cleanupTimeoutConfigurer)
	if !ok {
		return nil
	}

	timeout, err := parseCleanupTimeoutEnv(os.Getenv("SIDECAR_CLEANUP_TIMEOUT"))
	if err != nil {
		return err
	}
	configurable.SetCleanupTimeout(timeout)
	return nil
}

func parseCleanupTimeoutEnv(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultCleanupTimeout, nil
	}

	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0, errors.New("SIDECAR_CLEANUP_TIMEOUT must be non-negative")
		}
		return time.Duration(seconds) * time.Second, nil
	}

	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("SIDECAR_CLEANUP_TIMEOUT must be a duration or integer seconds")
	}
	if timeout < 0 {
		return 0, errors.New("SIDECAR_CLEANUP_TIMEOUT must be non-negative")
	}
	return timeout, nil
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

func newBackend(agentType string, cmd []string, cfg AgentConfig) (AgentBackend, error) {
	// AGENT_PROMPT triggers fire-and-forget (-p) mode.
	// If empty, the sidecar runs in interactive mode (default).
	// Value is base64-encoded to survive Docker env var restrictions (no newlines).
	prompt := decodePromptEnv(os.Getenv("AGENT_PROMPT"))

	switch agentType {
	case "claude":
		sessionID := cfg.ResumeSession // empty = generate fresh UUID in NewClaudeBackend
		resume := sessionID != ""      // resuming a prior session
		return NewClaudeBackend(ClaudeBackendConfig{
			Binary:       cmd[0],
			Prompt:       prompt,
			SessionID:    sessionID,
			Resume:       resume,
			Model:        cfg.Model,
			MaxTurns:     cfg.MaxTurns,
			AllowedTools: cfg.AllowedTools,
			Effort:       cfg.Effort,
			ExtraEnv:     cfg.Env,
		}), nil
	case "codex":
		if prompt != "" {
			return newCodexBackendPromptMode(cmd[0], prompt, cfg), nil
		}
		return newCodexBackendInteractive(cmd[0], cfg), nil
	default:
		return newGenericCommandBackend(agentType, cmd, prompt), nil
	}
}

func stallConfigFromAgentConfig(cfg AgentConfig) StallConfig {
	return StallConfig{
		WarningTimeout: durationFromSeconds(cfg.StallWarningTimeout, 600),
		KillTimeout:    durationFromSeconds(cfg.StallKillTimeout, 3000),
		ResultGrace:    durationFromSeconds(cfg.ResultGracePeriod, 10),
	}
}

// decodePromptEnv decodes a base64-encoded AGENT_PROMPT value.
// Prompts are base64-encoded by the runtime to survive Docker's no-newlines
// restriction on env var values. Returns empty string on empty input.
func decodePromptEnv(raw string) string {
	if raw == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Fallback: treat as plain text (supports old binaries / manual testing).
		return raw
	}
	return string(decoded)
}

// durationFromSeconds converts integer seconds to time.Duration.
// 0 means "use default". -1 means "disabled" (returns -1ns, which < 0).
func durationFromSeconds(seconds, defaultSeconds int) time.Duration {
	switch {
	case seconds < 0:
		return -1 // disabled; all checks use `> 0` guards
	case seconds == 0:
		return time.Duration(defaultSeconds) * time.Second
	default:
		return time.Duration(seconds) * time.Second
	}
}

