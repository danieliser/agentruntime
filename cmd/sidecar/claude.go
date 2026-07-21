package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type ClaudeBackendConfig struct {
	Binary           string
	SessionID        string
	Resume           bool // when true, pass --resume to continue prior session
	WorkspaceFolders []string
	StartProcess     ClaudeProcessStarter
	// Prompt mode: if set, runs claude -p "prompt" (fire-and-forget).
	// If empty, runs interactive mode with --input-format stream-json.
	Prompt string

	// Fields from AGENT_CONFIG passthrough.
	Model        string            // --model flag (e.g. "claude-opus-4-5")
	MaxTurns     int               // --max-turns flag
	AllowedTools []string          // --allowedTools flag (repeatable)
	Effort       string            // --effort flag
	SystemPrompt string            // --system-prompt override
	ExtraEnv     map[string]string // merged into buildCleanEnv

	// Team fields — enable Claude Code Agent Teams inbox protocol.
	TeamName      string // --team-name flag
	TeamAgentName string // --agent-name flag
	TeamAgentID   string // --agent-id flag

	// Bare mode — skip hooks, plugins, LSP, automem, CLAUDE.md (clean room).
	// VERIFIED 2026-07-21: --bare only works with ANTHROPIC_API_KEY auth;
	// subscription OAuth/keychain credentials are NOT loaded in bare mode.
	// Spawn fails fast if Bare is set without an API key available.
	Bare bool

	// CleanContext applies the verified isolation flag set.
	//
	// PROBE HISTORY (probing the model is the only reliable check):
	//   2026-07-20 audit: --system-prompt + settings override + empty strict
	//   mcp config + --no-chrome probed clean on the then-current CLI.
	//   2026-07-21 re-probe on claude 2.1.216: that set is NO LONGER clean —
	//   plugin-provided MCP servers, skills, and host CLAUDE.md all leaked.
	//   --safe-mode ("all customizations disabled") probed clean: NONE, and
	//   subscription OAuth survives (verified with no ANTHROPIC_API_KEY in
	//   env). --bare remains API-key-only.
	//
	// Clean context therefore uses --safe-mode, keeping the settings override
	// and empty strict MCP config as defense-in-depth against future CLI
	// drift. KNOWN RESIDUAL: the CLI's default bundled skills remain
	// ("claude-bundled-skills" in contamination metadata).
	CleanContext bool
}

type ClaudeSpawnSpec struct {
	Command string
	Args    []string
	Env     []string
	Dir     string
}

type ClaudeProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() error
	Kill() error
}

type ClaudeProcessStarter func(context.Context, ClaudeSpawnSpec) (ClaudeProcess, error)

type ClaudeBackend struct {
	binary    string
	sessionID string
	resume    bool
	workspace []string
	prompt    string // if set, fire-and-forget -p mode

	// AGENT_CONFIG passthrough fields.
	model        string
	maxTurns     int
	allowedTools []string
	effort       string
	systemPrompt string
	extraEnv     map[string]string

	// Team fields.
	teamName      string
	teamAgentName string
	teamAgentID   string

	bare         bool
	cleanContext bool

	// tempMCPConfig is the ephemeral empty MCP config written for clean-context
	// sessions; removed when the backend stops.
	tempMCPConfig string

	startProcess ClaudeProcessStarter

	mu       sync.RWMutex
	mcp      *MCPServer
	process  ClaudeProcess
	stdin    io.WriteCloser
	running  bool
	once     sync.Once
	doneOnce sync.Once

	events chan Event
	done   chan struct{}
	waitCh chan backendExit

	stderrMu sync.Mutex
	stderr   strings.Builder
}

type execClaudeProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

type claudeAssistantEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Content []claudeAssistantContent `json:"content"`
		Usage   struct {
			InputTokens              int `json:"input_tokens,omitempty"`
			OutputTokens             int `json:"output_tokens,omitempty"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
		} `json:"usage"`
	} `json:"message"`
}

type claudeAssistantContent struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

type claudeResultEnvelope struct {
	Type       string  `json:"type"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	SessionID  string  `json:"session_id,omitempty"`
	NumTurns   int     `json:"num_turns,omitempty"`
	Subtype    string  `json:"subtype,omitempty"`
}

func NewClaudeBackend(cfg ClaudeBackendConfig) *ClaudeBackend {
	workspace := append([]string(nil), cfg.WorkspaceFolders...)
	if len(workspace) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			workspace = []string{cwd}
		}
	}

	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	startProcess := cfg.StartProcess
	if startProcess == nil {
		startProcess = startExecClaudeProcess
	}

	return &ClaudeBackend{
		binary:        binary,
		sessionID:     sessionID,
		resume:        cfg.Resume,
		workspace:     workspace,
		prompt:        cfg.Prompt,
		model:         cfg.Model,
		maxTurns:      cfg.MaxTurns,
		allowedTools:  cfg.AllowedTools,
		effort:        cfg.Effort,
		systemPrompt:  cfg.SystemPrompt,
		extraEnv:      cfg.ExtraEnv,
		teamName:      cfg.TeamName,
		teamAgentName: cfg.TeamAgentName,
		teamAgentID:   cfg.TeamAgentID,
		bare:          cfg.Bare,
		cleanContext:  cfg.CleanContext,
		startProcess:  startProcess,
		events:        make(chan Event, 64),
		done:          make(chan struct{}),
		waitCh:        make(chan backendExit, 1),
	}
}

func (b *ClaudeBackend) Start(ctx context.Context) error {
	return b.Spawn(ctx)
}

func (b *ClaudeBackend) Spawn(ctx context.Context) error {
	var spawnErr error
	b.once.Do(func() {
		var args []string
		var envExtra []string

		// Bare mode requires API-key auth. VERIFIED 2026-07-21: --bare skips
		// OAuth/keychain credential loading entirely, so a session without
		// ANTHROPIC_API_KEY would fail with an opaque auth error mid-run.
		// Fail fast with an actionable message instead.
		if b.bare && !b.hasAnthropicAPIKey() {
			err := errors.New("bare mode requires ANTHROPIC_API_KEY: --bare does not load OAuth/keychain credentials; use clean-context mode for subscription auth")
			b.emitError(err.Error())
			spawnErr = err
			return
		}

		if b.prompt != "" {
			// Fire-and-forget: claude -p "prompt" — no MCP server needed
			args = []string{
				"-p", b.prompt,
				"--output-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
				"--dangerously-skip-permissions",
			}
			if b.resume {
				// --resume <session-id> continues a prior Claude session.
				args = append(args, "--resume", b.sessionID)
			} else {
				args = append(args, "--session-id", b.sessionID)
			}
		} else {
			// Interactive: start MCP server for tool support + context injection
			server, err := NewMCPServer(MCPServerConfig{
				WorkspaceFolders: b.workspace,
			})
			if err != nil {
				b.emitError(err.Error())
				spawnErr = err
				return
			}
			if err := server.Start(); err != nil {
				b.emitError(err.Error())
				spawnErr = err
				return
			}
			b.mu.Lock()
			b.mcp = server
			b.mu.Unlock()

			envExtra = server.EnvVars()
			args = []string{
				"--output-format", "stream-json",
				"--input-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
				"--dangerously-skip-permissions",
				"--ide",
			}
			if b.resume {
				args = append(args, "--resume", b.sessionID)
			} else {
				args = append(args, "--session-id", b.sessionID)
			}
		}

		if b.cleanContext {
			// Clean-context isolation. --safe-mode disables all
			// customizations (CLAUDE.md, skills, plugins, hooks, MCP servers)
			// while keeping subscription OAuth — probed clean 2026-07-21 on
			// claude 2.1.216. The empty strict MCP config stays as
			// defense-in-depth against future CLI drift.
			mcpPath, err := writeEmptyMCPConfig()
			if err != nil {
				b.emitError(err.Error())
				spawnErr = err
				return
			}
			b.tempMCPConfig = mcpPath
			args = append(args, "--safe-mode", "--mcp-config", mcpPath, "--strict-mcp-config", "--no-chrome")
		} else {
			// Load MCP servers from materialized .mcp.json if it exists.
			// --ide mode doesn't auto-discover .mcp.json, so we pass it explicitly.
			mcpConfigPath := filepath.Join(os.Getenv("HOME"), ".claude", ".mcp.json")
			if _, err := os.Stat(mcpConfigPath); err == nil {
				args = append(args, "--mcp-config", mcpConfigPath)
			}
		}

		// Append AGENT_CONFIG passthrough flags (apply to both modes).
		if b.model != "" {
			args = append(args, "--model", b.model)
		}
		if b.maxTurns > 0 {
			args = append(args, "--max-turns", strconv.Itoa(b.maxTurns))
		}
		for _, tool := range b.allowedTools {
			args = append(args, "--allowedTools", tool)
		}
		if b.effort != "" {
			args = append(args, "--effort", b.effort)
		}

		if b.systemPrompt != "" {
			args = append(args, "--system-prompt", b.systemPrompt)
		}

		if settings := b.buildSettingsOverride(); len(settings) > 0 {
			data, err := json.Marshal(settings)
			if err != nil {
				b.emitError(err.Error())
				spawnErr = err
				return
			}
			args = append(args, "--settings", string(data))
		}

		// Team flags — enable Agent Teams inbox protocol.
		if b.teamName != "" {
			args = append(args, "--agent-id", b.teamAgentID)
			args = append(args, "--agent-name", b.teamAgentName)
			args = append(args, "--team-name", b.teamName)
			envExtra = append(envExtra, "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1")
			envExtra = append(envExtra, "CLAUDECODE=1")
		}

		// Bare mode — clean room, skip hooks/plugins/LSP/automem/CLAUDE.md.
		if b.bare {
			args = append(args, "--bare")
		}

		// Build a clean environment — DO NOT inherit host env wholesale.
		// Only pass through essential vars + explicit extras (MCP server env).
		// This prevents host hooks, plugins, MCP servers from leaking in.
		// Merge AGENT_CONFIG.env on top of the clean env. Clean-context
		// sessions use the stricter allowlist (no XDG_* redirects, no
		// NODE_OPTIONS/NODE_PATH injection vectors).
		for k, v := range b.extraEnv {
			envExtra = append(envExtra, k+"="+v)
		}
		cleanEnv := buildCleanEnv(envExtra)
		if b.cleanContext {
			cleanEnv = buildCleanContextEnv(envExtra)
		}

		log.Printf("[claude] spawn: %s %v (resume=%v session=%s)", b.binary, redactPromptArgs(args, b.prompt), b.resume, b.sessionID)

		spec := ClaudeSpawnSpec{
			Command: b.binary,
			Args:    args,
			Env:     cleanEnv,
		}
		if len(b.workspace) > 0 {
			spec.Dir = b.workspace[0]
		}

		process, err := b.startProcess(ctx, spec)
		if err != nil {
			b.mu.RLock()
			mcp := b.mcp
			b.mu.RUnlock()
			if mcp != nil {
				_ = mcp.Stop()
			}
			b.emitError(err.Error())
			spawnErr = err
			return
		}

		stdin := process.Stdin()
		// Close stdin for prompt mode — claude -p waits for EOF before processing.
		// Interactive mode keeps stdin open for JSONL input.
		if b.prompt != "" && stdin != nil {
			stdin.Close()
			stdin = nil
		}

		b.mu.Lock()
		b.process = process
		b.stdin = stdin
		b.running = true
		b.mu.Unlock()

		go b.readStdout(process.Stdout())
		go b.readStderr(process.Stderr())
		go b.waitForExit(process)
	})
	return spawnErr
}

func (b *ClaudeBackend) Events() <-chan Event {
	return b.events
}

func (b *ClaudeBackend) SendPrompt(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return errors.New("prompt content is required")
	}
	return b.writeInput(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": content},
			},
		},
	})
}

func (b *ClaudeBackend) SendInterrupt() error {
	return b.writeInput(map[string]any{
		"type": "control_request",
		"request": map[string]any{
			"subtype": "interrupt",
		},
	})
}

func (b *ClaudeBackend) SendSteer(content string) error {
	if err := b.SendInterrupt(); err != nil {
		return err
	}
	return b.SendPrompt(content)
}

func (b *ClaudeBackend) SendContext(text, filePath string) error {
	server := b.currentMCP()
	if server == nil {
		return errors.New("mcp server unavailable")
	}
	return server.SendSelection(text, filePath, 0, 0)
}

func (b *ClaudeBackend) SendMention(filePath string, lineStart, lineEnd int) error {
	server := b.currentMCP()
	if server == nil {
		return errors.New("mcp server unavailable")
	}
	return server.SendAtMention(filePath, lineStart, lineEnd)
}

func (b *ClaudeBackend) Stop() error {
	var stopErr error

	b.mu.Lock()
	process := b.process
	server := b.mcp
	b.running = false
	b.mu.Unlock()

	if process != nil {
		if err := process.Kill(); err != nil {
			stopErr = err
		}
	}
	if server != nil {
		if err := server.Stop(); err != nil && stopErr == nil {
			stopErr = err
		}
	}
	b.removeTempMCPConfig()

	b.markDone()
	return stopErr
}

func (b *ClaudeBackend) Close() error {
	return b.Stop()
}

// markDone closes the done channel exactly once. Stop (API deletion) and
// waitForExit (natural process exit) race here — a bare select-then-close
// can panic on double close.
func (b *ClaudeBackend) markDone() {
	b.doneOnce.Do(func() { close(b.done) })
}

func (b *ClaudeBackend) SessionID() string {
	return b.sessionID
}

func (b *ClaudeBackend) Running() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

func (b *ClaudeBackend) Wait() <-chan backendExit {
	return b.waitCh
}

// Contamination reports context leakage this backend cannot strip.
// PROBED 2026-07-21: --safe-mode leaves only the CLI's default bundled
// skills (deep-research, dataviz, review, ... — they ship with the binary).
func (b *ClaudeBackend) Contamination() []string {
	if !b.cleanContext {
		return nil
	}
	return []string{"claude-bundled-skills"}
}

// PID returns the agent process PID, or 0 if not started.
func (b *ClaudeBackend) PID() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	type pider interface{ PID() int }
	if p, ok := b.process.(pider); ok {
		return p.PID()
	}
	return 0
}

// buildSettingsOverride assembles the --settings JSON for this session.
// Two independent concerns feed it:
//
//   - clean context: {"hooks":{},"statusLine":null,"enabledPlugins":{}} rides
//     along with --safe-mode as defense-in-depth. (On the 2026-07-20 CLI this
//     override was the primary mitigation; on 2.1.216 --safe-mode does the
//     heavy lifting and this is belt-and-suspenders.)
//   - effort pairing: VERIFIED 2026-07-20: --effort xhigh returns an API 400
//     unless settings include "alwaysThinkingEnabled": true. The adapter pairs
//     them automatically; "max" gets the same pairing (higher tier of the same
//     thinking requirement — xhigh is the empirically verified case).
func (b *ClaudeBackend) buildSettingsOverride() map[string]any {
	settings := map[string]any{}
	if b.cleanContext {
		settings["hooks"] = map[string]any{}
		settings["statusLine"] = nil
		settings["enabledPlugins"] = map[string]any{}
	}
	if b.effort == "xhigh" || b.effort == "max" {
		settings["alwaysThinkingEnabled"] = true
	}
	return settings
}

// writeEmptyMCPConfig writes {"mcpServers":{}} to a temp file for
// --mcp-config + --strict-mcp-config in clean-context mode.
func writeEmptyMCPConfig() (string, error) {
	f, err := os.CreateTemp("", "agentruntime-empty-mcp-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(`{"mcpServers":{}}`); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (b *ClaudeBackend) hasAnthropicAPIKey() bool {
	if b.extraEnv["ANTHROPIC_API_KEY"] != "" {
		return true
	}
	return os.Getenv("ANTHROPIC_API_KEY") != ""
}

func (b *ClaudeBackend) removeTempMCPConfig() {
	if b.tempMCPConfig != "" {
		_ = os.Remove(b.tempMCPConfig)
	}
}

func (b *ClaudeBackend) currentMCP() *MCPServer {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.mcp
}

func (b *ClaudeBackend) writeInput(payload map[string]any) error {
	b.mu.RLock()
	stdin := b.stdin
	b.mu.RUnlock()
	if stdin == nil {
		return errors.New("claude stdin unavailable")
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stdin == nil {
		return errors.New("claude stdin unavailable")
	}
	_, err = b.stdin.Write(append(data, '\n'))
	return err
}

func (b *ClaudeBackend) readStdout(r io.ReadCloser) {
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

func (b *ClaudeBackend) readStderr(r io.ReadCloser) {
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
			Data: map[string]any{
				"subtype": "stderr",
				"text":    text,
			},
		})
	}
}

func (b *ClaudeBackend) waitForExit(process ClaudeProcess) {
	err := process.Wait()

	b.mu.Lock()
	b.running = false
	server := b.mcp
	b.mu.Unlock()

	if server != nil {
		_ = server.Stop()
	}
	b.removeTempMCPConfig()

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

func (b *ClaudeBackend) handleStdoutLine(line []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		b.emit(Event{
			Type: "system",
			Data: map[string]any{
				"subtype": "stdout_raw",
				"text":    string(line),
			},
		})
		return
	}

	switch envelope.Type {
	case "assistant":
		b.handleAssistant(line)
	case "stream_event":
		b.handleStreamEvent(line)
	case "result":
		b.handleResult(line)
	case "progress":
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err == nil {
			b.emit(Event{Type: "progress", Data: payload})
		}
	case "system":
		b.handleSystem(line)
	case "control_request":
		b.handleControlRequest(line)
	}
}

// handleStreamEvent processes streaming token deltas from --include-partial-messages.
// Format: {"type":"stream_event","event":{"delta":{"type":"text_delta","text":"tok"}}}
func (b *ClaudeBackend) handleStreamEvent(line []byte) {
	var payload struct {
		Event struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(line, &payload); err != nil {
		return
	}
	if payload.Event.Delta.Type == "text_delta" && payload.Event.Delta.Text != "" {
		b.emit(Event{
			Type: "agent_message",
			Data: map[string]any{
				"text":  payload.Event.Delta.Text,
				"delta": true,
			},
		})
	}
}

func (b *ClaudeBackend) handleAssistant(line []byte) {
	var payload claudeAssistantEnvelope
	if err := json.Unmarshal(line, &payload); err != nil {
		return
	}

	textParts := make([]string, 0, len(payload.Message.Content))
	toolEvents := make([]Event, 0)
	for _, item := range payload.Message.Content {
		switch item.Type {
		case "text":
			textParts = append(textParts, item.Text)
		case "tool_use":
			toolEvents = append(toolEvents, Event{
				Type: "tool_use",
				Data: map[string]any{
					"id":    item.ID,
					"name":  item.Name,
					"input": item.Input,
				},
			})
		}
	}

	b.emit(Event{
		Type: "agent_message",
		Data: map[string]any{
			"text": strings.Join(textParts, ""),
			"usage": map[string]any{
				"input_tokens":                payload.Message.Usage.InputTokens,
				"output_tokens":               payload.Message.Usage.OutputTokens,
				"cache_read_input_tokens":     payload.Message.Usage.CacheReadInputTokens,
				"cache_creation_input_tokens": payload.Message.Usage.CacheCreationInputTokens,
			},
		},
	})

	for _, event := range toolEvents {
		b.emit(event)
	}
}

func (b *ClaudeBackend) handleResult(line []byte) {
	var payload claudeResultEnvelope
	if err := json.Unmarshal(line, &payload); err != nil {
		return
	}
	b.emit(Event{
		Type: "result",
		Data: map[string]any{
			"cost_usd":    payload.CostUSD,
			"duration_ms": payload.DurationMS,
			"session_id":  payload.SessionID,
			"num_turns":   payload.NumTurns,
			"subtype":     payload.Subtype,
		},
	})
}

func (b *ClaudeBackend) handleSystem(line []byte) {
	var payload map[string]any
	if err := json.Unmarshal(line, &payload); err != nil {
		return
	}
	subtype, _ := payload["subtype"].(string)
	if strings.HasPrefix(subtype, "hook_") {
		b.emit(Event{
			Type: "system",
			Data: map[string]any{"subtype": subtype},
		})
		return
	}
	b.emit(Event{Type: "system", Data: payload})
}

func (b *ClaudeBackend) handleControlRequest(line []byte) {
	var payload struct {
		Request struct {
			RequestID string `json:"request_id"`
			Subtype   string `json:"subtype"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &payload); err != nil {
		return
	}
	if payload.Request.Subtype != "can_use_tool" || payload.Request.RequestID == "" {
		return
	}
	_ = b.writeInput(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"request_id": payload.Request.RequestID,
			"behavior":   "allow",
		},
	})
}

func (b *ClaudeBackend) emit(event Event) {
	select {
	case <-b.done:
		return
	case b.events <- event:
	default:
		// The channel is intentionally buffered so a slow consumer does not
		// block Claude's stdout parsing. If the buffer fills, backpressure wins.
		b.events <- event
	}
}

func (b *ClaudeBackend) emitError(message string) {
	b.emit(Event{
		Type: "error",
		Data: map[string]any{"message": message},
	})
}

func (b *ClaudeBackend) appendStderr(text string) {
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

func (b *ClaudeBackend) stderrDetail() string {
	b.stderrMu.Lock()
	defer b.stderrMu.Unlock()
	return strings.TrimSpace(b.stderr.String())
}

func claudeExitCode(err error) int {
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func startExecClaudeProcess(ctx context.Context, spec ClaudeSpawnSpec) (ClaudeProcess, error) {
	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &execClaudeProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}, nil
}

func (p *execClaudeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execClaudeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execClaudeProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *execClaudeProcess) Wait() error           { return p.cmd.Wait() }
func (p *execClaudeProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *execClaudeProcess) PID() int {
	if p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}
