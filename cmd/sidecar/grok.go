package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// GrokBackendConfig configures the native xAI grok CLI backend.
//
// The backend is single-turn prompt mode only: `grok --always-approve
// --output-format streaming-json -p "<prompt>"`. The grok TUI has no
// pipe-friendly interactive protocol, so AGENT_PROMPT is required.
//
// ISOLATION (VERIFIED 2026-07-20): under the real HOME, grok reads
// ~/.claude/CLAUDE.md (and its @-imports), ~/.cursor/AGENTS.md, and connects
// to 9+ MCP servers from other tools' configs — the leakiest CLI surveyed.
// Auth is file-based (~/.grok/auth.json + agent_id), so a fake HOME containing
// only .grok/{auth.json,agent_id,config.toml} strips all of it. Residual:
// 4 built-in CLI skills that ship with the binary (acceptable).
type GrokBackendConfig struct {
	Binary           string
	Prompt           string
	WorkspaceFolders []string
	StartProcess     ClaudeProcessStarter // generic process starter, injectable for tests

	// AGENT_CONFIG passthrough fields.
	Model        string            // --model flag (e.g. "grok-4.5")
	Effort       string            // --reasoning-effort flag
	MaxTurns     int               // --max-turns flag
	SystemPrompt string            // --system-prompt-override flag
	Context      string            // "clean" = fake HOME materialization
	ExtraEnv     map[string]string // merged into buildCleanEnv
}

// grokEndEvent is the terminal event of a streaming-json run.
// Probed 2026-07-21 against grok 0.2.106:
//
//	{"type":"end","stopReason":"EndTurn","sessionId":"...","usage":{...},
//	 "num_turns":1,"total_cost_usd":0.0047,...}
type grokEndEvent struct {
	Type         string         `json:"type"`
	StopReason   string         `json:"stopReason"`
	SessionID    string         `json:"sessionId"`
	Usage        map[string]any `json:"usage"`
	NumTurns     int            `json:"num_turns"`
	TotalCostUSD float64        `json:"total_cost_usd"`
}

type GrokBackend struct {
	binary       string
	prompt       string
	workspace    []string
	model        string
	effort       string
	maxTurns     int
	systemPrompt string
	contextMode  string
	extraEnv     map[string]string

	startProcess ClaudeProcessStarter

	mu        sync.RWMutex
	process   ClaudeProcess
	running   bool
	sessionID string
	fakeHome  string // ephemeral HOME for clean-context sessions
	once      sync.Once
	doneOnce  sync.Once

	events chan Event
	done   chan struct{}
	waitCh chan backendExit

	stderrMu sync.Mutex
	stderr   strings.Builder
}

func NewGrokBackend(cfg GrokBackendConfig) *GrokBackend {
	binary := cfg.Binary
	if binary == "" {
		binary = "grok"
	}
	startProcess := cfg.StartProcess
	if startProcess == nil {
		startProcess = startExecClaudeProcess
	}
	workspace := append([]string(nil), cfg.WorkspaceFolders...)
	if len(workspace) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			workspace = []string{cwd}
		}
	}

	return &GrokBackend{
		binary:       binary,
		prompt:       cfg.Prompt,
		workspace:    workspace,
		model:        cfg.Model,
		effort:       cfg.Effort,
		maxTurns:     cfg.MaxTurns,
		systemPrompt: cfg.SystemPrompt,
		contextMode:  cfg.Context,
		extraEnv:     cfg.ExtraEnv,
		startProcess: startProcess,
		sessionID:    uuid.NewString(),
		events:       make(chan Event, 64),
		done:         make(chan struct{}),
		waitCh:       make(chan backendExit, 1),
	}
}

func (b *GrokBackend) Start(ctx context.Context) error {
	var startErr error
	b.once.Do(func() {
		if strings.TrimSpace(b.prompt) == "" {
			startErr = errors.New("grok backend requires AGENT_PROMPT: the grok CLI is single-turn prompt mode only")
			b.emitError(startErr.Error())
			return
		}

		args := []string{"--always-approve", "--output-format", "streaming-json"}
		if b.model != "" {
			args = append(args, "--model", b.model)
		}
		if b.effort != "" {
			args = append(args, "--reasoning-effort", b.effort)
		}
		if b.maxTurns > 0 {
			args = append(args, "--max-turns", strconv.Itoa(b.maxTurns))
		}
		if b.systemPrompt != "" {
			args = append(args, "--system-prompt-override", b.systemPrompt)
		}
		args = append(args, "-p", b.prompt)

		var envExtra []string
		for k, v := range b.extraEnv {
			envExtra = append(envExtra, k+"="+v)
		}

		buildEnv := buildCleanEnv
		if b.contextMode == "clean" {
			fakeHome, err := materializeGrokFakeHome()
			if err != nil {
				b.emitError(err.Error())
				startErr = err
				return
			}
			b.mu.Lock()
			b.fakeHome = fakeHome
			b.mu.Unlock()
			// Last duplicate wins in exec.Cmd.Env — this overrides the
			// passthrough HOME from the allowlist.
			envExtra = append(envExtra, "HOME="+fakeHome)
			buildEnv = buildCleanContextEnv
		}

		log.Printf("[grok] spawn: %s %v (session=%s clean=%v)", b.binary, redactPromptArgs(args, b.prompt), b.sessionID, b.contextMode == "clean")

		spec := ClaudeSpawnSpec{
			Command: b.binary,
			Args:    args,
			Env:     buildEnv(envExtra),
		}
		if len(b.workspace) > 0 {
			spec.Dir = b.workspace[0]
		}

		process, err := b.startProcess(ctx, spec)
		if err != nil {
			b.removeFakeHome()
			b.emitError(err.Error())
			startErr = err
			return
		}

		// Single-turn mode never reads stdin; close it defensively so grok
		// can never block waiting for input (same failure class as codex).
		if stdin := process.Stdin(); stdin != nil {
			_ = stdin.Close()
		}

		b.mu.Lock()
		b.process = process
		b.running = true
		b.mu.Unlock()

		go b.readStdout(process.Stdout())
		go b.readStderr(process.Stderr())
		go b.waitForExit(process)
	})
	return startErr
}

func (b *GrokBackend) SendPrompt(string) error {
	return errors.New("grok backend is single-turn: additional prompts are not supported")
}

// SendInterrupt kills the process — grok headless has no interrupt channel.
func (b *GrokBackend) SendInterrupt() error {
	b.mu.RLock()
	process := b.process
	b.mu.RUnlock()
	if process == nil {
		return nil
	}
	return process.Kill()
}

func (b *GrokBackend) SendSteer(string) error {
	return errors.New("grok backend does not support steering")
}

func (b *GrokBackend) SendContext(string, string) error {
	return errors.New("grok backend does not support context injection")
}

func (b *GrokBackend) SendMention(string, int, int) error {
	return errors.New("grok backend does not support mentions")
}

func (b *GrokBackend) Events() <-chan Event { return b.events }

func (b *GrokBackend) SessionID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sessionID
}

func (b *GrokBackend) Running() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

func (b *GrokBackend) Wait() <-chan backendExit { return b.waitCh }

func (b *GrokBackend) Close() error {
	b.mu.Lock()
	process := b.process
	b.running = false
	b.mu.Unlock()

	var closeErr error
	if process != nil {
		closeErr = process.Kill()
	}
	b.removeFakeHome()

	b.markDone()
	return closeErr
}

// markDone closes the done channel exactly once. Close (API deletion) and
// waitForExit (natural process exit) race here — a bare select-then-close
// can panic on double close.
func (b *GrokBackend) markDone() {
	b.doneOnce.Do(func() { close(b.done) })
}

// Contamination reports context leakage this backend cannot strip.
// The fake-HOME strip leaves grok's 4 built-in CLI skills (check-work,
// create-skill, help, imagine) — they ship with the binary.
func (b *GrokBackend) Contamination() []string {
	if b.contextMode != "clean" {
		return nil
	}
	return []string{"grok-builtin-skills"}
}

// PID returns the agent process PID, or 0 if not started.
func (b *GrokBackend) PID() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	type pider interface{ PID() int }
	if p, ok := b.process.(pider); ok {
		return p.PID()
	}
	return 0
}

func (b *GrokBackend) readStdout(r io.ReadCloser) {
	if r == nil {
		return
	}
	defer r.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		b.handleStdoutLine(line)
	}
}

// handleStdoutLine normalizes grok streaming-json (probed 2026-07-21):
//
//	{"type":"thought","data":"<delta>"}  reasoning token delta
//	{"type":"text","data":"<delta>"}     response token delta
//	{"type":"end", ...}                  terminal event with usage/cost
//
// LIMITATION: grok 0.2.106 emits NO tool events in streaming-json — tool
// activity (file writes, shell commands) happens silently between text
// deltas. Consumers cannot observe tool_use/tool_result for this backend.
func (b *GrokBackend) handleStdoutLine(line []byte) {
	var envelope struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		b.emit(Event{
			Type: "system",
			Data: map[string]any{"subtype": "stdout_raw", "text": string(line)},
		})
		return
	}

	switch envelope.Type {
	case "text":
		b.emit(Event{
			Type: "agent_message",
			Data: map[string]any{"text": envelope.Data, "delta": true},
		})
	case "thought":
		b.emit(Event{
			Type: "agent_message",
			Data: map[string]any{"text": envelope.Data, "delta": true, "thought": true},
		})
	case "end":
		b.handleEnd(line)
	case "error":
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err == nil {
			b.emit(Event{Type: "error", Data: payload})
		}
	default:
		// Forward unknown event types as-is (future CLI versions may add
		// tool events — do not drop them silently).
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err == nil {
			b.emit(Event{Type: envelope.Type, Data: payload})
		}
	}
}

func (b *GrokBackend) handleEnd(line []byte) {
	var payload grokEndEvent
	if err := json.Unmarshal(line, &payload); err != nil {
		return
	}
	if payload.SessionID != "" {
		b.mu.Lock()
		b.sessionID = payload.SessionID
		b.mu.Unlock()
	}
	b.emit(Event{
		Type: "result",
		Data: map[string]any{
			"session_id":  payload.SessionID,
			"status":      payload.StopReason,
			"cost_usd":    payload.TotalCostUSD,
			"num_turns":   payload.NumTurns,
			"usage":       payload.Usage,
			"duration_ms": int64(0), // grok does not report duration
		},
	})
}

func (b *GrokBackend) readStderr(r io.ReadCloser) {
	if r == nil {
		return
	}
	defer r.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 16*1024), 256*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		b.appendStderr(text)
		b.emit(Event{
			Type: "system",
			Data: map[string]any{"subtype": "stderr", "text": text},
		})
	}
}

func (b *GrokBackend) waitForExit(process ClaudeProcess) {
	err := process.Wait()

	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	b.removeFakeHome()

	code := 0
	detail := ""
	if err != nil {
		code = claudeExitCode(err)
		detail = b.stderrDetail()
		if detail == "" {
			detail = err.Error()
		}
	}
	select {
	case b.waitCh <- backendExit{Code: code, ErrorDetail: detail}:
	default:
	}

	b.markDone()
	close(b.waitCh)
	close(b.events)
}

func (b *GrokBackend) emit(event Event) {
	select {
	case <-b.done:
		return
	case b.events <- event:
	default:
		b.events <- event
	}
}

func (b *GrokBackend) emitError(message string) {
	b.emit(Event{
		Type: "error",
		Data: map[string]any{"message": message},
	})
}

func (b *GrokBackend) appendStderr(text string) {
	const maxStderrDetailBytes = 8 * 1024

	b.stderrMu.Lock()
	defer b.stderrMu.Unlock()

	if text == "" {
		return
	}
	if b.stderr.Len() > 0 {
		b.stderr.WriteByte('\n')
	}
	b.stderr.WriteString(text)
	if b.stderr.Len() <= maxStderrDetailBytes {
		return
	}

	trimmed := b.stderr.String()
	trimmed = trimmed[len(trimmed)-maxStderrDetailBytes:]
	b.stderr.Reset()
	b.stderr.WriteString(trimmed)
}

func (b *GrokBackend) stderrDetail() string {
	b.stderrMu.Lock()
	defer b.stderrMu.Unlock()
	return strings.TrimSpace(b.stderr.String())
}

func (b *GrokBackend) removeFakeHome() {
	b.mu.Lock()
	fakeHome := b.fakeHome
	b.fakeHome = ""
	b.mu.Unlock()
	if fakeHome != "" {
		_ = os.RemoveAll(fakeHome)
	}
}

// materializeGrokFakeHome builds the VERIFIED clean fake HOME for grok:
// only .grok/{auth.json,agent_id,config.toml} (+ models_cache.json when
// present). The real ~/.grok is read, never written. auth.json is required —
// grok auth is purely file-based, so without it the CLI lands in the login
// flow and hangs headless.
func materializeGrokFakeHome() (string, error) {
	realGrok := filepath.Join(os.Getenv("HOME"), ".grok")
	authSrc := filepath.Join(realGrok, "auth.json")
	if _, err := os.Stat(authSrc); err != nil {
		return "", errors.New("grok clean context requires " + authSrc + ": grok auth is file-based and no auth.json was found")
	}

	fakeHome, err := os.MkdirTemp("", "agentruntime-grok-home-")
	if err != nil {
		return "", err
	}
	grokDir := filepath.Join(fakeHome, ".grok")
	if err := os.MkdirAll(grokDir, 0o700); err != nil {
		os.RemoveAll(fakeHome)
		return "", err
	}

	// auth.json is required; agent_id and models_cache.json are best-effort.
	if err := copyGrokFile(authSrc, filepath.Join(grokDir, "auth.json")); err != nil {
		os.RemoveAll(fakeHome)
		return "", err
	}
	for _, name := range []string{"agent_id", "models_cache.json"} {
		src := filepath.Join(realGrok, name)
		if _, err := os.Stat(src); err == nil {
			if err := copyGrokFile(src, filepath.Join(grokDir, name)); err != nil {
				os.RemoveAll(fakeHome)
				return "", err
			}
		}
	}

	// Minimal config — matches the probe-verified recipe. Never copy the
	// real config.toml (it may carry MCP servers and instruction wiring).
	config := "[cli]\ninstaller = \"internal\"\nauto_update = false\n[ui]\nyolo = false\n"
	if err := os.WriteFile(filepath.Join(grokDir, "config.toml"), []byte(config), 0o600); err != nil {
		os.RemoveAll(fakeHome)
		return "", err
	}
	return fakeHome, nil
}

func copyGrokFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
