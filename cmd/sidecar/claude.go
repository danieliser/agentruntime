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
	ExtraEnv     map[string]string // merged into buildCleanEnv
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
	extraEnv     map[string]string

	startProcess ClaudeProcessStarter

	mu      sync.RWMutex
	mcp     *MCPServer
	process ClaudeProcess
	stdin   io.WriteCloser
	running bool
	once    sync.Once

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
		binary:       binary,
		sessionID:    sessionID,
		resume:       cfg.Resume,
		workspace:    workspace,
		prompt:       cfg.Prompt,
		model:        cfg.Model,
		maxTurns:     cfg.MaxTurns,
		allowedTools: cfg.AllowedTools,
		effort:       cfg.Effort,
		extraEnv:     cfg.ExtraEnv,
		startProcess: startProcess,
		events:       make(chan Event, 64),
		done:         make(chan struct{}),
		waitCh:       make(chan backendExit, 1),
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
				args = append(args, "--resume", "--session-id", b.sessionID)
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
				args = append(args, "--resume", "--session-id", b.sessionID)
			} else {
				args = append(args, "--session-id", b.sessionID)
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

		// Build a clean environment — DO NOT inherit host env wholesale.
		// Only pass through essential vars + explicit extras (MCP server env).
		// This prevents host hooks, plugins, MCP servers from leaking in.
		// Merge AGENT_CONFIG.env on top of the clean env.
		for k, v := range b.extraEnv {
			envExtra = append(envExtra, k+"="+v)
		}
		cleanEnv := buildCleanEnv(envExtra)

		log.Printf("[claude] spawn: %s %v (resume=%v session=%s)", b.binary, args, b.resume, b.sessionID)

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

	select {
	case <-b.done:
	default:
		close(b.done)
	}
	return stopErr
}

func (b *ClaudeBackend) Close() error {
	return b.Stop()
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

	select {
	case <-b.done:
	default:
		close(b.done)
	}
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

// buildCleanEnv creates a minimal environment for the agent process.
// Only essential system vars are inherited. No host hooks, plugins, or
// MCP servers leak through. Extra vars (e.g., IDE MCP env) are appended.
func buildCleanEnv(extra []string) []string {
	// Essential vars that the agent process needs to function.
	passthrough := []string{
		"PATH", "HOME", "USER", "LANG", "TERM",
		"SHELL", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		// Claude OAuth (if set by the credential sync or env-file)
		"CLAUDE_CODE_OAUTH_TOKEN",
		// Codex / OpenAI auth
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		// Node/npm (needed by Claude Code CLI)
		"NODE_PATH", "NODE_OPTIONS", "NVM_DIR",
		// Proxy (set by network manager)
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
	}

	env := make([]string, 0, len(passthrough)+len(extra))
	hostEnv := make(map[string]string)
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			hostEnv[e[:i]] = e[i+1:]
		}
	}

	for _, key := range passthrough {
		if val, ok := hostEnv[key]; ok {
			env = append(env, key+"="+val)
		}
	}

	// NOTE: In Docker containers, HOME=/home/agent with our clean .claude/ mount.
	// When running locally, Claude reads the host's ~/.claude/ (hooks, plugins, etc).
	// This is expected — the Docker container IS the isolation boundary.

	// Append explicit extras (MCP server env vars, etc.)
	env = append(env, extra...)

	return env
}
