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
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// CursorBackendConfig configures the cursor-agent CLI backend.
//
// Prompt mode only: `cursor-agent --print --output-format stream-json
// --force --trust [--model m] "<prompt>"`. The cursor TUI has no
// pipe-friendly interactive protocol.
//
// AUTH: macOS keychain ("cursor-access-token"). A fake HOME breaks auth
// unless $FAKEHOME/Library/Keychains symlinks to the real keychain
// (VERIFIED working 2026-07-20). CURSOR_API_KEY env auth bypasses this.
//
// KNOWN CONTAMINATION (not strippable): 4+ account-level user rules sync
// SERVER-SIDE and inject into every session regardless of local config.
// The adapter surfaces this via Contamination() so consumers can decide
// whether the route is acceptable — do not fight it locally.
type CursorBackendConfig struct {
	Binary           string
	Prompt           string
	WorkspaceFolders []string
	StartProcess     ClaudeProcessStarter // generic process starter, injectable for tests

	// AGENT_CONFIG passthrough fields.
	Model    string            // --model flag; effort is encoded in the model string, e.g. "claude-opus-4-8[effort=high]"
	Context  string            // "clean" = fake HOME + keychain symlink
	ExtraEnv map[string]string // merged into buildCleanEnv
}

type CursorBackend struct {
	binary      string
	prompt      string
	workspace   []string
	model       string
	contextMode string
	extraEnv    map[string]string

	startProcess ClaudeProcessStarter

	mu        sync.RWMutex
	process   ClaudeProcess
	running   bool
	sessionID string
	fakeHome  string
	once      sync.Once
	doneOnce  sync.Once

	events chan Event
	done   chan struct{}
	waitCh chan backendExit

	// eventsMu serializes sends on events against its close in waitForExit.
	// markDone() runs before the close, so an emit blocked on a full buffer
	// exits via done and releases the lock first.
	eventsMu     sync.Mutex
	eventsClosed bool

	stderrMu sync.Mutex
	stderr   strings.Builder
}

// cursorEnvelope covers the stream-json event types probed 2026-07-21
// against cursor-agent 2026.07.17:
//
//	{"type":"system","subtype":"init","session_id":...,"model":...}
//	{"type":"user","message":{...}}                       (prompt echo)
//	{"type":"thinking","subtype":"delta","text":"..."}
//	{"type":"thinking","subtype":"completed"}
//	{"type":"assistant","message":{"content":[{"type":"text","text":..}]}}
//	{"type":"tool_call","subtype":"started"|"completed","call_id":...,
//	 "tool_call":{"editToolCall":{"args":{...},"result":{...}}}}
//	{"type":"result","subtype":"success","is_error":false,"result":"...",
//	 "duration_ms":...,"usage":{"inputTokens":...,"outputTokens":...}}
type cursorEnvelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Text      string `json:"text"`
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	ToolCall   map[string]json.RawMessage `json:"tool_call"`
	IsError    bool                       `json:"is_error"`
	Result     string                     `json:"result"`
	DurationMS int64                      `json:"duration_ms"`
	Usage      map[string]any             `json:"usage"`
}

func NewCursorBackend(cfg CursorBackendConfig) *CursorBackend {
	binary := cfg.Binary
	if binary == "" {
		binary = "cursor-agent"
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

	return &CursorBackend{
		binary:       binary,
		prompt:       cfg.Prompt,
		workspace:    workspace,
		model:        cfg.Model,
		contextMode:  cfg.Context,
		extraEnv:     cfg.ExtraEnv,
		startProcess: startProcess,
		sessionID:    uuid.NewString(),
		events:       make(chan Event, 64),
		done:         make(chan struct{}),
		waitCh:       make(chan backendExit, 1),
	}
}

// Contamination reports context leakage that this backend CANNOT strip.
// Cursor account-level user rules are synced server-side into every session.
func (b *CursorBackend) Contamination() []string {
	return []string{"cursor-account-rules"}
}

func (b *CursorBackend) Start(ctx context.Context) error {
	var startErr error
	b.once.Do(func() {
		if strings.TrimSpace(b.prompt) == "" {
			startErr = errors.New("cursor backend requires AGENT_PROMPT: cursor-agent is prompt mode only")
			b.emitError(startErr.Error())
			return
		}

		args := []string{"--print", "--output-format", "stream-json", "--force", "--trust"}
		if b.model != "" {
			args = append(args, "--model", b.model)
		}
		args = append(args, b.prompt)

		var envExtra []string
		for k, v := range b.extraEnv {
			envExtra = append(envExtra, k+"="+v)
		}

		buildEnv := buildCleanEnv
		if b.contextMode == "clean" {
			fakeHome, err := materializeCursorFakeHome()
			if err != nil {
				b.emitError(err.Error())
				startErr = err
				return
			}
			b.mu.Lock()
			b.fakeHome = fakeHome
			b.mu.Unlock()
			envExtra = append(envExtra, "HOME="+fakeHome)
			buildEnv = buildCleanContextEnv
		}

		log.Printf("[cursor] spawn: %s %v (session=%s clean=%v)", b.binary, redactPromptArgs(args, b.prompt), b.sessionID, b.contextMode == "clean")

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

		// Prompt mode never reads stdin; close it so the CLI can never block.
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

func (b *CursorBackend) SendPrompt(string) error {
	return errors.New("cursor backend is single-turn: additional prompts are not supported")
}

// SendInterrupt kills the process — cursor headless has no interrupt channel.
func (b *CursorBackend) SendInterrupt() error {
	b.mu.RLock()
	process := b.process
	b.mu.RUnlock()
	if process == nil {
		return nil
	}
	return process.Kill()
}

func (b *CursorBackend) SendSteer(string) error {
	return errors.New("cursor backend does not support steering")
}

func (b *CursorBackend) SendContext(string, string) error {
	return errors.New("cursor backend does not support context injection")
}

func (b *CursorBackend) SendMention(string, int, int) error {
	return errors.New("cursor backend does not support mentions")
}

func (b *CursorBackend) Events() <-chan Event { return b.events }

func (b *CursorBackend) SessionID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sessionID
}

func (b *CursorBackend) Running() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

func (b *CursorBackend) Wait() <-chan backendExit { return b.waitCh }

func (b *CursorBackend) Close() error {
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
func (b *CursorBackend) markDone() {
	b.doneOnce.Do(func() { close(b.done) })
}

// PID returns the agent process PID, or 0 if not started.
func (b *CursorBackend) PID() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	type pider interface{ PID() int }
	if p, ok := b.process.(pider); ok {
		return p.PID()
	}
	return 0
}

func (b *CursorBackend) readStdout(r io.ReadCloser) {
	if r == nil {
		return
	}
	defer r.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		b.handleStdoutLine(line)
	}
}

func (b *CursorBackend) handleStdoutLine(line []byte) {
	var envelope cursorEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		b.emit(Event{
			Type: "system",
			Data: map[string]any{"subtype": "stdout_raw", "text": string(line)},
		})
		return
	}

	if envelope.SessionID != "" {
		b.mu.Lock()
		b.sessionID = envelope.SessionID
		b.mu.Unlock()
	}

	switch envelope.Type {
	case "system":
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err == nil {
			b.emit(Event{Type: "system", Data: payload})
		}
	case "user":
		// Prompt echo — skip, the caller already knows what it sent.
	case "thinking":
		if envelope.Subtype == "delta" && envelope.Text != "" {
			b.emit(Event{
				Type: "agent_message",
				Data: map[string]any{"text": envelope.Text, "delta": true, "thought": true},
			})
		}
	case "assistant":
		textParts := make([]string, 0, len(envelope.Message.Content))
		for _, item := range envelope.Message.Content {
			if item.Type == "text" {
				textParts = append(textParts, item.Text)
			}
		}
		if len(textParts) > 0 {
			b.emit(Event{
				Type: "agent_message",
				Data: map[string]any{"text": strings.Join(textParts, "")},
			})
		}
	case "tool_call":
		b.handleToolCall(envelope)
	case "result":
		b.emit(Event{
			Type: "result",
			Data: map[string]any{
				"session_id":  envelope.SessionID,
				"status":      envelope.Subtype,
				"is_error":    envelope.IsError,
				"duration_ms": envelope.DurationMS,
				"usage":       normalizeCursorUsage(envelope.Usage),
				"text":        envelope.Result,
			},
		})
	default:
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err == nil {
			b.emit(Event{Type: envelope.Type, Data: payload})
		}
	}
}

// handleToolCall maps cursor tool_call events to tool_use / tool_result.
// The tool_call payload holds exactly one "<name>ToolCall" key, e.g.
// "editToolCall" → tool name "edit".
func (b *CursorBackend) handleToolCall(envelope cursorEnvelope) {
	name := ""
	var payload map[string]any
	for key, raw := range envelope.ToolCall {
		if strings.HasSuffix(key, "ToolCall") {
			name = strings.TrimSuffix(key, "ToolCall")
			_ = json.Unmarshal(raw, &payload)
			break
		}
	}

	switch envelope.Subtype {
	case "started":
		input, _ := payload["args"].(map[string]any)
		b.emit(Event{
			Type: "tool_use",
			Data: map[string]any{
				"id":    envelope.CallID,
				"name":  name,
				"input": input,
			},
		})
	case "completed":
		// cursor reports outcomes as result.{success|error}; per-tool
		// duration is not present in the stream, so duration_ms stays absent.
		isError := false
		if result, ok := payload["result"].(map[string]any); ok {
			_, isError = result["error"]
		}
		b.emit(Event{
			Type: "tool_result",
			Data: map[string]any{
				"id":       envelope.CallID,
				"name":     name,
				"output":   payload["result"],
				"is_error": isError,
			},
		})
	}
}

// normalizeCursorUsage maps cursor's camelCase usage keys onto the snake_case
// shape every result-event consumer expects (sessionio metrics, dashboard,
// TUI, startup-crash detection). Unrecognized keys pass through untouched so
// future CLI additions are not dropped.
func normalizeCursorUsage(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	renames := map[string]string{
		"inputTokens":         "input_tokens",
		"outputTokens":        "output_tokens",
		"cacheReadTokens":     "cache_read_input_tokens",
		"cachedInputTokens":   "cache_read_input_tokens",
		"cacheWriteTokens":    "cache_creation_input_tokens",
		"cacheCreationTokens": "cache_creation_input_tokens",
	}
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		if renamed, ok := renames[key]; ok {
			out[renamed] = value
			continue
		}
		out[key] = value
	}
	return out
}

func (b *CursorBackend) readStderr(r io.ReadCloser) {
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

func (b *CursorBackend) waitForExit(process ClaudeProcess) {
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
	b.eventsMu.Lock()
	b.eventsClosed = true
	close(b.events)
	b.eventsMu.Unlock()
}

func (b *CursorBackend) emit(event Event) {
	b.eventsMu.Lock()
	defer b.eventsMu.Unlock()
	if b.eventsClosed {
		return
	}
	select {
	case <-b.done:
		return
	case b.events <- event:
	}
}

func (b *CursorBackend) emitError(message string) {
	b.emit(Event{
		Type: "error",
		Data: map[string]any{"message": message},
	})
}

func (b *CursorBackend) appendStderr(text string) {
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

func (b *CursorBackend) stderrDetail() string {
	b.stderrMu.Lock()
	defer b.stderrMu.Unlock()
	return strings.TrimSpace(b.stderr.String())
}

func (b *CursorBackend) removeFakeHome() {
	b.mu.Lock()
	fakeHome := b.fakeHome
	b.fakeHome = ""
	b.mu.Unlock()
	if fakeHome != "" {
		_ = os.RemoveAll(fakeHome)
	}
}

// materializeCursorFakeHome builds a fake HOME for cursor-agent. Auth lives
// in the macOS keychain, so $FAKEHOME/Library/Keychains symlinks to the real
// keychain directory (VERIFIED working 2026-07-20). Nothing else is copied —
// in particular NOT ~/.cursor (its AGENTS.md and MCP config are exactly the
// leak being stripped). On non-darwin platforms the symlink is skipped;
// CURSOR_API_KEY env auth is the supported path there.
func materializeCursorFakeHome() (string, error) {
	fakeHome, err := os.MkdirTemp("", "agentruntime-cursor-home-")
	if err != nil {
		return "", err
	}

	if runtime.GOOS == "darwin" {
		realKeychains := filepath.Join(os.Getenv("HOME"), "Library", "Keychains")
		if _, err := os.Stat(realKeychains); err == nil {
			libDir := filepath.Join(fakeHome, "Library")
			if err := os.MkdirAll(libDir, 0o700); err != nil {
				os.RemoveAll(fakeHome)
				return "", err
			}
			if err := os.Symlink(realKeychains, filepath.Join(libDir, "Keychains")); err != nil {
				os.RemoveAll(fakeHome)
				return "", err
			}
		}
	}
	return fakeHome, nil
}
