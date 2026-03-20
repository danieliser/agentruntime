package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// LocalSidecarRuntime spawns agent processes via a local sidecar binary.
// Same protocol as Docker runtime (sidecar WS), but no container — the
// sidecar and agent run directly on the host. This gives local runtime
// the same structured output, streaming deltas, and unified event shapes
// as Docker runtime.
type LocalSidecarRuntime struct {
	// SidecarBin is the path to the sidecar binary.
	// Defaults to "agentruntime-sidecar" (must be in PATH or built locally).
	SidecarBin string
}

// NewLocalSidecarRuntime creates a local sidecar runtime.
func NewLocalSidecarRuntime() *LocalSidecarRuntime {
	return &LocalSidecarRuntime{}
}

func (r *LocalSidecarRuntime) Name() string                    { return "local" }
func (r *LocalSidecarRuntime) Cleanup(_ context.Context) error { return nil }

func (r *LocalSidecarRuntime) sidecarBinary() string {
	if r.SidecarBin != "" {
		return r.SidecarBin
	}
	// Try to find the sidecar binary
	if path, err := exec.LookPath("agentruntime-sidecar"); err == nil {
		return path
	}
	// Fall back to building from source location
	return "agentruntime-sidecar"
}

// Spawn starts a local sidecar subprocess and connects to it via WebSocket.
func (r *LocalSidecarRuntime) Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error) {
	if len(cfg.Cmd) == 0 {
		return nil, &SpawnError{Reason: "cmd is empty"}
	}

	port, err := findFreePort()
	if err != nil {
		return nil, &SpawnError{Reason: "find free port", Err: err}
	}

	// Build AGENT_CMD — just the binary name for interactive mode
	agentCmd, err := json.Marshal([]string{cfg.Cmd[0]})
	if err != nil {
		return nil, &SpawnError{Reason: "marshal agent cmd", Err: err}
	}

	// Start sidecar subprocess with a clean environment.
	// AI provider API keys are stripped so agents use OAuth credentials
	// (materialized .credentials.json / auth.json) instead of hitting
	// API rate limits from the operator's personal key.
	sidecar := exec.CommandContext(ctx, r.sidecarBinary())
	sidecar.Dir = cfg.WorkDir
	sidecar.Env = append(cleanLocalEnv(),
		fmt.Sprintf("AGENT_CMD=%s", agentCmd),
		fmt.Sprintf("SIDECAR_PORT=%d", port),
	)
	// Pass prompt for fire-and-forget mode.
	// Base64-encoded to match Docker runtime encoding (and sidecar decodes both).
	if cfg.Prompt != "" {
		sidecar.Env = append(sidecar.Env, fmt.Sprintf("AGENT_PROMPT=%s", base64.StdEncoding.EncodeToString([]byte(cfg.Prompt))))
	}
	// Pass agent config (model, resume_session, env, etc.) to sidecar
	if acJSON := buildAgentConfigJSON(cfg); acJSON != "" {
		sidecar.Env = append(sidecar.Env, fmt.Sprintf("AGENT_CONFIG=%s", acJSON))
	}
	// Silence sidecar's own logging
	sidecar.Stdout = os.Stderr
	sidecar.Stderr = os.Stderr

	if err := sidecar.Start(); err != nil {
		return nil, &SpawnError{Reason: "start sidecar", Err: err}
	}

	// Health check — wait for sidecar to be ready
	healthURL := fmt.Sprintf("http://localhost:%d/health", port)
	deadline := time.Now().Add(15 * time.Second)
	healthy := false
	lastHTTPDetail := ""
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthURL)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				var health struct {
					Status      string `json:"status"`
					AgentType   string `json:"agent_type"`
					ErrorDetail string `json:"error_detail"`
				}
				decodeErr := json.NewDecoder(resp.Body).Decode(&health)
				_ = resp.Body.Close()
				if decodeErr == nil && health.Status == "error" {
					_ = sidecar.Process.Kill()
					return nil, &SpawnError{Reason: "sidecar health", Err: fmt.Errorf("sidecar health check failed: %s", health.ErrorDetail)}
				}
				if decodeErr == nil && health.AgentType != "" {
					lastHTTPDetail = ""
					healthy = true
					break
				}
			} else {
				lastHTTPDetail = fmt.Sprintf("status %s: %s", resp.Status, httpResponseBody(resp))
				_ = resp.Body.Close()
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !healthy && time.Now().After(deadline) {
		_ = sidecar.Process.Kill()
		if lastHTTPDetail == "" {
			lastHTTPDetail = "timed out waiting for sidecar health"
		}
		return nil, &SpawnError{Reason: "sidecar health", Err: errors.New(lastHTTPDetail)}
	}

	// Connect WS — prompt is sent via AGENT_PROMPT env, not WS
	handle, err := dialSidecar(
		fmt.Sprintf("local-sidecar-%d", sidecar.Process.Pid),
		fmt.Sprintf("%d", port),
		0,
		"", // prompt already set via AGENT_PROMPT env
	)
	if err != nil {
		_ = sidecar.Process.Kill()
		return nil, &SpawnError{Reason: "dial sidecar ws", Err: err}
	}

	// Send prompt via WS if provided
	if cfg.Prompt != "" {
		// Prompt mode — sidecar uses AGENT_PROMPT env, no WS prompt needed
	}

	// Override kill to also stop the sidecar process
	handle.killFn = func() error {
		if sidecar.Process != nil {
			return sidecar.Process.Kill()
		}
		return nil
	}

	return handle, nil
}

// Recover returns empty — local sidecar processes don't survive daemon restart.
func (r *LocalSidecarRuntime) Recover(_ context.Context) ([]ProcessHandle, error) {
	return nil, nil
}

// strippedEnvKeys are environment variables that must NOT be inherited by
// agent sidecar processes. AI provider API keys override OAuth credentials
// in Claude Code and Codex, causing agents to hit API rate limits instead
// of using subscription pricing. Agents authenticate via materialized
// credential files (.credentials.json, auth.json), not env vars.
var strippedEnvKeys = map[string]bool{
	"ANTHROPIC_API_KEY": true,
	"OPENAI_API_KEY":    true,
	"CODEX_API_KEY":     true,
}

// cleanLocalEnv returns the host environment with AI provider API keys removed.
// This matches the Docker runtime's clean-room model: agents use OAuth via
// materialized credential files, never the operator's personal API keys.
func cleanLocalEnv() []string {
	env := os.Environ()
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if strippedEnvKeys[key] {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

func findFreePort() (int, error) {
	// Bind to :0 and immediately close to get a free port
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		// Fallback to random port in high range
		return 10000 + rand.Intn(55000), nil
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}
