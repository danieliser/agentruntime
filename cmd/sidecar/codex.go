package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"

	"github.com/google/uuid"
)

var errCodexNoActiveTurn = errors.New("codex backend has no active turn")

type codexBackend struct {
	binary       string
	prompt       string // if set, fire-and-forget exec mode
	model        string // --model flag override
	approvalMode string // "full-auto" | "auto-edit" | "suggest"
	extraEnv     map[string]string
	logger       *log.Logger
	spawner      codexSpawner
	sessionID    string

	mu           sync.RWMutex
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	closeFn      func() error
	ctx          context.Context
	cancel       context.CancelFunc
	threadID     string
	activeTurnID string
	nextID       int64
	started      bool
	running      bool

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan codexRPCResponse

	events    chan Event
	waitCh    chan backendExit
	done      chan struct{}
	closeOnce sync.Once

	stderrMu  sync.Mutex
	stderrBuf strings.Builder
}

type codexSpawner func(ctx context.Context, cmd []string) (*codexTransport, error)

type codexTransport struct {
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	wait    <-chan error
	closeFn func() error
}

type codexRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type codexRPCMessage struct {
	Method string          `json:"method,omitempty"`
	ID     any             `json:"id,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexRPCResponse struct {
	Result json.RawMessage
	Err    error
}

func newCodexBackend() *codexBackend {
	return newCodexBackendWithBinary("codex")
}

func newCodexBackendWithBinary(binary string) *codexBackend {
	return newCodexBackendConfig(binary, "", log.Default(), spawnCodexAppServer, AgentConfig{})
}

// newCodexBackendPromptMode creates a fire-and-forget backend using codex exec.
func newCodexBackendPromptMode(binary, prompt string, cfg AgentConfig) *codexBackend {
	return newCodexBackendConfig(binary, prompt, log.Default(), spawnCodexAppServer, cfg)
}

// newCodexBackendInteractive creates an interactive backend with AGENT_CONFIG fields.
func newCodexBackendInteractive(binary string, cfg AgentConfig) *codexBackend {
	return newCodexBackendConfig(binary, "", log.Default(), spawnCodexAppServer, cfg)
}

func newCodexBackendWithSpawner(logger *log.Logger, spawner codexSpawner) *codexBackend {
	return newCodexBackendConfig("codex", "", logger, spawner, AgentConfig{})
}

func newCodexBackendConfig(binary, prompt string, logger *log.Logger, spawner codexSpawner, cfg AgentConfig) *codexBackend {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if spawner == nil {
		spawner = spawnCodexAppServer
	}
	if binary == "" {
		binary = "codex"
	}

	return &codexBackend{
		binary:       binary,
		prompt:       prompt,
		model:        cfg.Model,
		approvalMode: cfg.ApprovalMode,
		extraEnv:     cfg.Env,
		logger:       logger,
		spawner:      spawner,
		sessionID:    uuid.NewString(),
		pending:      make(map[string]chan codexRPCResponse),
		events:       make(chan Event, 64),
		waitCh:       make(chan backendExit, 1),
		done:         make(chan struct{}),
		nextID:       1,
	}
}

func spawnCodexAppServer(ctx context.Context, cmdArgs []string) (*codexTransport, error) {
	if len(cmdArgs) == 0 {
		return nil, errors.New("missing codex command")
	}

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
		close(waitCh)
	}()

	return &codexTransport{
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		wait:   waitCh,
		closeFn: func() error {
			if cmd.Process == nil {
				return nil
			}
			return cmd.Process.Kill()
		},
	}, nil
}

func (b *codexBackend) Start(ctx context.Context) error {
	if b.prompt != "" {
		return b.startPromptMode(ctx)
	}
	return b.Spawn(ctx)
}

// startPromptMode runs codex exec --json for fire-and-forget tasks.
// Output is JSONL on stdout (same event types as app-server notifications).
// No JSON-RPC handshake, no stdin input, no steering.
func (b *codexBackend) startPromptMode(ctx context.Context) error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return nil
	}
	b.started = true
	b.running = true
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.mu.Unlock()

	approvalFlag := "--full-auto"
	if b.approvalMode != "" && b.approvalMode != "full-auto" {
		approvalFlag = "--" + b.approvalMode
	}
	cmd := []string{b.binary, "exec", "--json", approvalFlag, "--skip-git-repo-check"}
	if b.model != "" {
		cmd = append(cmd, "--model", b.model)
	}
	cmd = append(cmd, b.prompt)
	transport, err := b.spawner(b.ctx, cmd)
	if err != nil {
		b.setRunning(false)
		b.emitError(err.Error())
		return err
	}

	b.mu.Lock()
	b.stdin = transport.stdin
	b.stdout = transport.stdout
	b.stderr = transport.stderr
	b.closeFn = transport.closeFn
	b.mu.Unlock()

	// Close stdin — exec mode doesn't need it
	if transport.stdin != nil {
		transport.stdin.Close()
	}

	// Use JSONL reader (not JSON-RPC) — codex exec --json outputs flat events
	go b.readExecJSONL()
	go b.readStderr()
	if transport.wait != nil {
		go b.waitLoop(transport.wait)
	}

	return nil
}

// readExecJSONL reads flat JSONL events from codex exec --json.
// Format: {"type":"thread.started",...}, {"type":"item.completed",...}, etc.
// Dot-delimited types (thread.started → thread/started for mapping).
func (b *codexBackend) readExecJSONL() {
	b.mu.RLock()
	stdout := b.stdout
	b.mu.RUnlock()
	if stdout == nil {
		return
	}

	decoder := json.NewDecoder(stdout)
	for {
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			if !errors.Is(err, io.EOF) && !b.isClosed() {
				b.emit(Event{Type: "error", Data: map[string]any{"message": err.Error()}})
			}
			return
		}

		eventType, _ := raw["type"].(string)
		// Map codex exec event types to our unified types
		switch {
		case strings.HasPrefix(eventType, "item.completed"):
			item, _ := raw["item"].(map[string]any)
			itemType, _ := item["type"].(string)
			switch itemType {
			case "agent_message":
				text, _ := item["text"].(string)
				b.emit(Event{Type: "agent_message", Data: map[string]any{
					"text": text, "final": true, "item": item,
				}})
			default:
				b.emit(Event{Type: "tool_result", Data: raw})
			}
		case strings.HasPrefix(eventType, "item.started"):
			item, _ := raw["item"].(map[string]any)
			itemType, _ := item["type"].(string)
			if itemType != "agent_message" {
				b.emit(Event{Type: "tool_use", Data: raw})
			}
		case eventType == "turn.completed":
			b.emit(Event{Type: "result", Data: raw})
		case eventType == "turn.started":
			// skip
		case eventType == "thread.started":
			b.emit(Event{Type: "system", Data: map[string]any{"subtype": "thread_started", "thread": raw}})
		case eventType == "error":
			b.emit(Event{Type: "error", Data: raw})
		default:
			// Forward unknown events as-is
			b.emit(Event{Type: eventType, Data: raw})
		}
	}
}

func (b *codexBackend) Spawn(ctx context.Context) error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return nil
	}
	b.started = true
	b.running = true
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.mu.Unlock()

	spawnCmd := []string{b.binary, "app-server", "--listen", "stdio://"}
	if b.model != "" {
		spawnCmd = append(spawnCmd, "--model", b.model)
	}
	transport, err := b.spawner(b.ctx, spawnCmd)
	if err != nil {
		b.setRunning(false)
		b.emitError(err.Error())
		return err
	}

	b.mu.Lock()
	b.stdin = transport.stdin
	b.stdout = transport.stdout
	b.stderr = transport.stderr
	b.closeFn = transport.closeFn
	b.mu.Unlock()

	go b.readLoop()
	go b.readStderr()
	if transport.wait != nil {
		go b.waitLoop(transport.wait)
	}

	result, err := b.callWithID(0, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "agentruntime",
			"version": "0.3.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	})
	if err != nil {
		b.emitError(err.Error())
		_ = b.Close()
		return err
	}

	userAgent := ""
	if decoded := decodeMap(result); decoded != nil {
		userAgent = stringField(decoded, "userAgent", "user_agent")
	}
	if userAgent == "" {
		b.emitError("codex initialize missing userAgent")
		_ = b.Close()
		return errors.New("codex initialize missing userAgent")
	}

	if err := b.notify("initialized", nil); err != nil {
		b.emitError(err.Error())
		_ = b.Close()
		return err
	}

	return nil
}

func (b *codexBackend) Events() <-chan Event {
	return b.events
}

func (b *codexBackend) SessionID() string {
	return b.sessionID
}

func (b *codexBackend) Running() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

func (b *codexBackend) Wait() <-chan backendExit {
	return b.waitCh
}

func (b *codexBackend) SendPrompt(content string) error {
	threadID, err := b.ensureThread()
	if err != nil {
		return err
	}

	_, err = b.call("turn/start", map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{
				"type": "text",
				"text": content,
			},
		},
		"approvalPolicy": b.codexApprovalPolicy(),
		"sandboxPolicy": map[string]any{
			"type": "dangerFullAccess",
		},
	})
	return err
}

func (b *codexBackend) SendSteer(content string) error {
	b.mu.RLock()
	threadID := b.threadID
	turnID := b.activeTurnID
	b.mu.RUnlock()

	if threadID == "" || turnID == "" {
		return errCodexNoActiveTurn
	}

	_, err := b.call("turn/steer", map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{
				"type": "text",
				"text": content,
			},
		},
		"expectedTurnId": turnID,
	})
	return err
}

func (b *codexBackend) SendInterrupt() error {
	b.mu.RLock()
	threadID := b.threadID
	b.mu.RUnlock()
	if threadID == "" {
		return nil
	}
	_, err := b.call("turn/interrupt", map[string]any{
		"threadId": threadID,
		"reason":   "user",
	})
	return err
}

func (b *codexBackend) SendContext(text, filePath string) error {
	if b.logger != nil {
		b.logger.Printf("warning: codex app-server does not support sidecar context injection: %q %q", filePath, text)
	}
	return nil
}

func (b *codexBackend) SendMention(filePath string, lineStart, lineEnd int) error {
	if b.logger != nil {
		b.logger.Printf("warning: codex app-server does not support sidecar mentions: %q:%d-%d", filePath, lineStart, lineEnd)
	}
	return nil
}

func (b *codexBackend) Close() error {
	var closeErr error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		cancel := b.cancel
		closeFn := b.closeFn
		stdin := b.stdin
		b.stdin = nil
		b.closeFn = nil
		b.running = false
		b.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if stdin != nil {
			_ = stdin.Close()
		}
		if closeFn != nil {
			closeErr = closeFn()
		}

		b.failPending(errors.New("codex backend closed"))
		close(b.done)
		close(b.events)
	})
	return closeErr
}

func (b *codexBackend) ensureThread() (string, error) {
	b.mu.RLock()
	threadID := b.threadID
	b.mu.RUnlock()
	if threadID != "" {
		return threadID, nil
	}

	result, err := b.call("thread/start", map[string]any{})
	if err != nil {
		return "", err
	}

	decoded := decodeMap(result)
	threadID = stringField(decoded, "threadId", "thread_id", "id")
	// Codex may nest the ID under a "thread" object: {"thread":{"id":"..."}}
	if threadID == "" {
		if threadObj, ok := decoded["thread"].(map[string]any); ok {
			threadID = stringField(threadObj, "id", "threadId", "thread_id")
		}
	}
	if threadID == "" {
		return "", fmt.Errorf("codex thread/start missing threadId in response: %v", decoded)
	}

	b.mu.Lock()
	if b.threadID == "" {
		b.threadID = threadID
	}
	threadID = b.threadID
	b.mu.Unlock()
	return threadID, nil
}

func (b *codexBackend) call(method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.mu.Unlock()
	return b.callWithID(id, method, params)
}

func (b *codexBackend) callWithID(id any, method string, params any) (json.RawMessage, error) {
	respCh := make(chan codexRPCResponse, 1)
	key := rpcIDKey(id)

	b.pendingMu.Lock()
	b.pending[key] = respCh
	b.pendingMu.Unlock()

	rawParams, err := marshalRawMessage(params)
	if err != nil {
		b.pendingMu.Lock()
		delete(b.pending, key)
		b.pendingMu.Unlock()
		return nil, err
	}

	if err := b.writeMessage(codexRPCMessage{
		Method: method,
		ID:     id,
		Params: rawParams,
	}); err != nil {
		b.pendingMu.Lock()
		delete(b.pending, key)
		b.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-b.contextDone():
		return nil, errors.New("codex backend closed")
	case resp, ok := <-respCh:
		if !ok {
			return nil, errors.New("codex backend closed")
		}
		return resp.Result, resp.Err
	}
}

func (b *codexBackend) notify(method string, params any) error {
	rawParams, err := marshalRawMessage(params)
	if err != nil {
		return err
	}
	return b.writeMessage(codexRPCMessage{
		Method: method,
		Params: rawParams,
	})
}

func (b *codexBackend) writeMessage(msg codexRPCMessage) error {
	b.mu.RLock()
	stdin := b.stdin
	b.mu.RUnlock()
	if stdin == nil {
		return errors.New("codex backend stdin unavailable")
	}

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	return json.NewEncoder(stdin).Encode(msg)
}

func (b *codexBackend) readLoop() {
	b.mu.RLock()
	stdout := b.stdout
	b.mu.RUnlock()
	if stdout == nil {
		return
	}

	decoder := json.NewDecoder(stdout)
	for {
		var msg codexRPCMessage
		if err := decoder.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) || b.isClosed() {
				b.failPending(errors.New("codex backend closed"))
				return
			}
			b.emit(Event{Type: "error", Data: map[string]any{"message": err.Error()}})
			b.failPending(err)
			return
		}

		switch {
		case msg.Method != "" && msg.ID != nil:
			b.handleServerRequest(msg)
		case msg.Method != "":
			b.handleNotification(msg)
		case msg.ID != nil:
			b.handleResponse(msg)
		}
	}
}

func (b *codexBackend) readStderr() {
	b.mu.RLock()
	stderr := b.stderr
	b.mu.RUnlock()
	if stderr == nil {
		return
	}
	defer stderr.Close()

	scanner := bufio.NewScanner(stderr)
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

func (b *codexBackend) waitLoop(wait <-chan error) {
	err, ok := <-wait
	if !ok || b.isClosed() {
		return
	}

	code := 0
	detail := ""
	if err != nil {
		code = codexExitCode(err)
		detail = b.stderrDetail()
		if detail == "" {
			detail = err.Error()
		}
		if !strings.Contains(err.Error(), "killed") {
			b.emitError(err.Error())
		}
	}

	select {
	case b.waitCh <- backendExit{Code: code, ErrorDetail: detail}:
	default:
	}

	_ = b.Close()
}

func (b *codexBackend) handleResponse(msg codexRPCMessage) {
	key := rpcIDKey(msg.ID)

	b.pendingMu.Lock()
	ch, ok := b.pending[key]
	if ok {
		delete(b.pending, key)
	}
	b.pendingMu.Unlock()
	if !ok {
		return
	}

	resp := codexRPCResponse{Result: msg.Result}
	if msg.Error != nil {
		resp.Err = fmt.Errorf("codex rpc error %d: %s", msg.Error.Code, msg.Error.Message)
	}

	ch <- resp
	close(ch)
}

func (b *codexBackend) handleServerRequest(msg codexRPCMessage) {
	if strings.Contains(msg.Method, "requestApproval") {
		result, err := marshalRawMessage(map[string]any{
			"decision": "accept",
		})
		if err != nil {
			b.emit(Event{Type: "error", Data: map[string]any{"message": "marshal approval: " + err.Error()}})
			return
		}
		_ = b.writeMessage(codexRPCMessage{
			ID:     msg.ID,
			Result: result,
		})
	}
}

func (b *codexBackend) handleNotification(msg codexRPCMessage) {
	params := decodeMap(msg.Params)
	switch msg.Method {
	case "thread/started":
		threadID := stringField(params, "threadId", "thread_id", "id")
		if threadID != "" {
			b.mu.Lock()
			b.threadID = threadID
			b.mu.Unlock()
		}
		eventData := cloneMap(params)
		eventData["subtype"] = "thread_started"
		b.emit(Event{Type: "system", Data: eventData})
	case "turn/started":
		turnID := stringField(params, "turnId", "turn_id", "id")
		if turnID != "" {
			b.mu.Lock()
			b.activeTurnID = turnID
			b.mu.Unlock()
		}
	case "turn/completed":
		b.mu.Lock()
		b.activeTurnID = ""
		b.mu.Unlock()
		b.emit(Event{Type: "result", Data: cloneMap(params)})
	case "item/agentMessage/delta":
		// Extract turnId from message events as a fallback — turn/started
		// notification may arrive after the first delta.
		if turnID := stringField(params, "turnId", "turn_id"); turnID != "" {
			b.mu.Lock()
			if b.activeTurnID == "" {
				b.activeTurnID = turnID
			}
			b.mu.Unlock()
		}
		eventData := cloneMap(params)
		if text := firstNonEmpty(stringField(params, "text", "delta"), nestedStringField(params, "item", "text")); text != "" {
			eventData["text"] = text
		}
		eventData["final"] = false
		b.emit(Event{Type: "agent_message", Data: eventData})
	case "item/started":
		itemType := normalizeItemType(firstNonEmpty(
			nestedStringField(params, "item", "type"),
			stringField(params, "type"),
		))
		if isCodexToolItem(itemType) {
			eventData := cloneMap(params)
			eventData["tool_type"] = itemType
			b.emit(Event{Type: "tool_use", Data: eventData})
		}
	case "item/completed":
		itemType := normalizeItemType(firstNonEmpty(
			nestedStringField(params, "item", "type"),
			stringField(params, "type"),
		))
		switch itemType {
		case "agent_message":
			eventData := cloneMap(params)
			if text := firstNonEmpty(
				stringField(params, "text"),
				nestedStringField(params, "item", "text"),
			); text != "" {
				eventData["text"] = text
			}
			eventData["final"] = true
			b.emit(Event{Type: "agent_message", Data: eventData})
		case "command_execution", "file_change", "mcp_tool_call":
			eventData := cloneMap(params)
			eventData["tool_type"] = itemType
			b.emit(Event{Type: "tool_result", Data: eventData})
		}
	case "error":
		b.emit(Event{Type: "error", Data: cloneMap(params)})
	}
}

func (b *codexBackend) emit(event Event) {
	select {
	case <-b.contextDone():
		return
	case b.events <- event:
	}
}

func (b *codexBackend) emitError(message string) {
	b.emit(Event{
		Type: "error",
		Data: map[string]any{"message": message},
	})
}

func (b *codexBackend) appendStderr(text string) {
	const maxStderrDetailBytes = 8 * 1024

	b.stderrMu.Lock()
	defer b.stderrMu.Unlock()

	if text == "" {
		return
	}
	if b.stderrBuf.Len() > 0 {
		b.stderrBuf.WriteByte('\n')
	}
	b.stderrBuf.WriteString(text)
	if b.stderrBuf.Len() <= maxStderrDetailBytes {
		return
	}

	trimmed := b.stderrBuf.String()
	trimmed = trimmed[len(trimmed)-maxStderrDetailBytes:]
	b.stderrBuf.Reset()
	b.stderrBuf.WriteString(trimmed)
}

func (b *codexBackend) stderrDetail() string {
	b.stderrMu.Lock()
	defer b.stderrMu.Unlock()
	return strings.TrimSpace(b.stderrBuf.String())
}

func codexExitCode(err error) int {
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func (b *codexBackend) failPending(err error) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	for key, ch := range b.pending {
		ch <- codexRPCResponse{Err: err}
		close(ch)
		delete(b.pending, key)
	}
}

func (b *codexBackend) contextDone() <-chan struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.ctx == nil {
		return closedChan()
	}
	return b.ctx.Done()
}

func (b *codexBackend) isClosed() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// codexApprovalPolicy maps the AGENT_CONFIG approval_mode to the Codex
// JSON-RPC approvalPolicy value used in turn/start.
func (b *codexBackend) codexApprovalPolicy() string {
	switch b.approvalMode {
	case "suggest":
		return "always"
	case "auto-edit":
		return "on-failure"
	default:
		// "full-auto" or empty — never ask for approval.
		return "never"
	}
}

func (b *codexBackend) setRunning(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running = v
}

func rpcIDKey(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

func marshalRawMessage(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal raw message: %w", err)
	}
	return data, nil
}

func decodeMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

func cloneMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return map[string]any{}
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func stringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			switch s := v.(type) {
			case string:
				if s != "" {
					return s
				}
			case json.Number:
				return s.String()
			}
		}
	}
	return ""
}

func nestedStringField(m map[string]any, parent string, keys ...string) string {
	nested, _ := m[parent].(map[string]any)
	return stringField(nested, keys...)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func normalizeItemType(raw string) string {
	if raw == "" {
		return ""
	}
	var out strings.Builder
	for i, r := range raw {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('_')
			}
			out.WriteRune(r + ('a' - 'A'))
			continue
		}
		if r == '-' || r == ' ' {
			out.WriteByte('_')
			continue
		}
		out.WriteRune(r)
	}
	return strings.ToLower(out.String())
}

func isCodexToolItem(itemType string) bool {
	switch itemType {
	case "command_execution", "file_change", "mcp_tool_call":
		return true
	default:
		return false
	}
}

var (
	closedOnce sync.Once
	closedC    chan struct{}
)

func closedChan() <-chan struct{} {
	closedOnce.Do(func() {
		closedC = make(chan struct{})
		close(closedC)
	})
	return closedC
}
