package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type ClaudeBackendConfig struct {
	Binary           string
	SessionID        string
	WorkspaceFolders []string
	StartProcess     ClaudeProcessStarter
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
	workspace []string

	startProcess ClaudeProcessStarter

	mu      sync.RWMutex
	mcp     *MCPServer
	process ClaudeProcess
	stdin   io.WriteCloser
	running bool
	once    sync.Once

	events chan Event
	done   chan struct{}
	waitCh chan int
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
		workspace:    workspace,
		startProcess: startProcess,
		events:       make(chan Event, 64),
		done:         make(chan struct{}),
		waitCh:       make(chan int, 1),
	}
}

func (b *ClaudeBackend) Start(ctx context.Context) error {
	return b.Spawn(ctx)
}

func (b *ClaudeBackend) Spawn(ctx context.Context) error {
	var spawnErr error
	b.once.Do(func() {
		server, err := NewMCPServer(MCPServerConfig{
			WorkspaceFolders: b.workspace,
		})
		if err != nil {
			spawnErr = err
			return
		}
		if err := server.Start(); err != nil {
			spawnErr = err
			return
		}

		spec := ClaudeSpawnSpec{
			Command: b.binary,
			Args: []string{
				"--output-format", "stream-json",
				"--input-format", "stream-json",
				"--verbose",
				"--dangerously-skip-permissions",
				"--ide",
				"--session-id", b.sessionID,
			},
			Env: append(os.Environ(), server.EnvVars()...),
		}
		if len(b.workspace) > 0 {
			spec.Dir = b.workspace[0]
		}

		process, err := b.startProcess(ctx, spec)
		if err != nil {
			_ = server.Stop()
			spawnErr = err
			return
		}

		b.mu.Lock()
		b.mcp = server
		b.process = process
		b.stdin = process.Stdin()
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

func (b *ClaudeBackend) Wait() <-chan int {
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
	if err != nil {
		code = 1
	}
	select {
	case b.waitCh <- code:
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
