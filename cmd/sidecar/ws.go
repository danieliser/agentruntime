package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	agentErrors "github.com/danieliser/agentruntime/pkg/errors"
	"github.com/danieliser/agentruntime/pkg/session"
)

const (
	replayBufferSize = 1 << 20
	writeTimeout     = 5 * time.Second
	pingInterval     = 30 * time.Second
	pongTimeout      = 10 * time.Second
)

var heartbeatInterval = 10 * time.Second // exported for testing

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

type Event struct {
	Type      string `json:"type"`
	Data      any    `json:"data"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Offset    int64  `json:"offset"`
	Timestamp int64  `json:"timestamp"`
}

type Command struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type AgentBackend interface {
	Start(ctx context.Context) error
	SendPrompt(content string) error
	SendInterrupt() error
	SendSteer(content string) error
	SendContext(text, filePath string) error
	SendMention(filePath string, lineStart, lineEnd int) error
	Events() <-chan Event
	SessionID() string
	Running() bool
	Wait() <-chan backendExit
}

type healthResponse struct {
	Status       string `json:"status"`
	AgentRunning bool   `json:"agent_running"`
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id"`
	ErrorDetail  string `json:"error_detail,omitempty"`
}

type backendExit struct {
	Code        int    `json:"code"`
	ErrorDetail string `json:"error_detail,omitempty"`
}

type ExternalWSServer struct {
	agentType string
	backend   AgentBackend
	replay    *session.ReplayBuffer

	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	startMu   sync.RWMutex
	startErr  error
	startTime time.Time // for uptime calculation

	appendMu sync.Mutex

	clientsMu sync.RWMutex
	clients   map[*wsClient]struct{}

	cleanupMu      sync.Mutex
	cleanupTimeout time.Duration
	cleanupTimer   *time.Timer
	cleanupID      uint64
	agentExited    bool
	shutdownFn     func()

	// Session metrics for error classification at exit.
	metricsMu        sync.Mutex
	totalInputToks   int
	totalOutputToks  int
	totalOutputBytes int
	totalToolCalls   int             // count of tool_use events
	agentTextBuf     strings.Builder // agent_message text for error scanning
	stderrAuthErrors int             // consecutive auth errors from stderr
	fatalEmitted     bool            // true after fatal error event sent

	// Stall detection state.
	stallDetector *StallDetector
	stallConfig   StallConfig
}

type wsClient struct {
	conn *websocket.Conn

	writeMu   sync.Mutex
	closeOnce sync.Once
}

type promptCommand struct {
	Content string `json:"content"`
}

type steerCommand struct {
	Content string `json:"content"`
}

type contextCommand struct {
	Text     string `json:"text"`
	FilePath string `json:"filePath"`
}

type mentionCommand struct {
	FilePath  string `json:"filePath"`
	LineStart int    `json:"lineStart"`
	LineEnd   int    `json:"lineEnd"`
}

type errorData struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type exitData struct {
	Code          int    `json:"code"`
	ErrorDetail   string `json:"error_detail,omitempty"`
	ErrorCategory string `json:"error_category,omitempty"`
	Retryable     bool   `json:"retryable,omitempty"`
}

type rawCommand struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func NewExternalWSServer(agentType string, backend AgentBackend, stallCfg StallConfig) *ExternalWSServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &ExternalWSServer{
		agentType:      agentType,
		backend:        backend,
		replay:         session.NewReplayBuffer(replayBufferSize),
		ctx:            ctx,
		cancel:         cancel,
		clients:        make(map[*wsClient]struct{}),
		cleanupTimeout: defaultCleanupTimeout,
		stallConfig:    stallCfg,
	}
}

func (s *ExternalWSServer) AgentType() string {
	return s.agentType
}

func (s *ExternalWSServer) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ws", s.handleWS)
	return mux
}

func (s *ExternalWSServer) SetCleanupTimeout(timeout time.Duration) {
	if timeout < 0 {
		timeout = 0
	}

	s.cleanupMu.Lock()
	s.cleanupTimeout = timeout
	s.cleanupMu.Unlock()
}

func (s *ExternalWSServer) SetShutdownFunc(fn func()) {
	s.cleanupMu.Lock()
	s.shutdownFn = fn
	s.cleanupMu.Unlock()
}

func (s *ExternalWSServer) Close() error {
	s.cancel()
	s.stopCleanupTimer()
	s.replay.Close()

	for _, client := range s.snapshotClients() {
		client.close()
	}

	switch backend := any(s.backend).(type) {
	case interface{ Close() error }:
		return backend.Close()
	case interface{ Stop() error }:
		return backend.Stop()
	default:
		return nil
	}
}

func (s *ExternalWSServer) Interrupt() error {
	if s.backend == nil {
		return nil
	}
	return s.backend.SendInterrupt()
}

func (s *ExternalWSServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	status := "ok"
	errorDetail := ""
	if err := s.getStartErr(); err != nil {
		status = "error"
		errorDetail = err.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:       status,
		AgentRunning: s.backend != nil && s.backend.Running(),
		AgentType:    s.agentType,
		SessionID:    s.sessionID(),
		ErrorDetail:  errorDetail,
	})
}

func (s *ExternalWSServer) handleWS(w http.ResponseWriter, r *http.Request) {
	since, hasSince, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		http.Error(w, "invalid since", http.StatusBadRequest)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := &wsClient{conn: conn}
	if err := s.ensureStarted(); err != nil {
		_ = client.writeJSON(Event{
			Type: "error",
			Data: errorData{
				Message: err.Error(),
				Code:    http.StatusInternalServerError,
			},
		})
		client.close()
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
	})

	if err := s.registerClient(client, hasSince, since); err != nil {
		client.close()
		return
	}
	defer s.unregisterClient(client)
	defer client.close()

	go client.pingLoop(s.ctx)
	s.readLoop(client)
}

func (s *ExternalWSServer) ensureStarted() error {
	s.startOnce.Do(func() {
		if s.backend == nil {
			s.setStartErr(errors.New("backend unavailable"))
			return
		}

		if err := s.backend.Start(s.ctx); err != nil {
			s.setStartErr(err)
			return
		}

		// Record start time for uptime calculation.
		s.startMu.Lock()
		s.startTime = time.Now()
		s.startMu.Unlock()

		// Create and start stall detector.
		s.stallDetector = NewStallDetector(
			s.stallConfig,
			s.recordAndBroadcast,
			s.cancel,
		)

		go s.eventLoop()
		go s.exitLoop()
		go s.stallDetector.Run(s.ctx)
		go s.systemEventHeartbeat()
	})
	return s.getStartErr()
}

func (s *ExternalWSServer) eventLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-s.backend.Events():
			if !ok {
				return
			}
			if event.Type == "" {
				continue
			}

			// Update stall detection timestamps.
			if s.stallDetector != nil {
				s.stallDetector.RecordEvent(event.Type)
			}

			// Track metrics before normalization (usage field is in raw data).
			fatal := s.trackEventMetrics(event)
			event = s.normalizeEvent(event)
			_ = s.recordAndBroadcast(event)
			if fatal {
				s.emitFatalError()
			}
		}
	}
}

// trackEventMetrics accumulates session-level metrics for error classification at exit.
// Returns true if a fatal error was detected and the session should be killed.
func (s *ExternalWSServer) trackEventMetrics(event Event) bool {
	data, _ := event.Data.(map[string]any)
	if data == nil {
		return false
	}

	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()

	switch event.Type {
	case "agent_message":
		if text, ok := data["text"].(string); ok {
			s.agentTextBuf.WriteString(text)
			s.agentTextBuf.WriteByte('\n')
			s.totalOutputBytes += len(text)
			// Check agent messages for fatal errors too.
			if cat := agentErrors.Classify(text); cat.Fatal() {
				s.stderrAuthErrors++
			}
		}
	case "tool_use":
		s.totalToolCalls++
	case "result":
		if usage, ok := data["usage"].(map[string]any); ok {
			if v, ok := usage["input_tokens"].(float64); ok {
				s.totalInputToks += int(v)
			}
			if v, ok := usage["output_tokens"].(float64); ok {
				s.totalOutputToks += int(v)
			}
		}
	case "system":
		// Scan stderr for auth/credential errors.
		if subtype, _ := data["subtype"].(string); subtype == "stderr" {
			if text, ok := data["text"].(string); ok {
				if cat := agentErrors.Classify(text); cat.Fatal() {
					s.stderrAuthErrors++
				} else {
					// Non-auth stderr resets the consecutive counter.
					s.stderrAuthErrors = 0
				}
			}
		}
	case "error":
		// Check error events from the agent backend.
		if msg, ok := data["message"].(string); ok {
			if cat := agentErrors.Classify(msg); cat.Fatal() {
				s.stderrAuthErrors++
			}
		}
	}

	// Threshold: 3+ consecutive auth errors = fatal. Kill the session.
	return s.stderrAuthErrors >= 3 && !s.fatalEmitted
}

// emitFatalError broadcasts a fatal error event and kills the agent process.
// Called when unrecoverable auth/credential errors are detected mid-session.
func (s *ExternalWSServer) emitFatalError() {
	s.metricsMu.Lock()
	if s.fatalEmitted {
		s.metricsMu.Unlock()
		return
	}
	s.fatalEmitted = true
	agentText := s.agentTextBuf.String()
	s.metricsMu.Unlock()

	// Classify to get the specific category and action message.
	cat := agentErrors.Classify(agentText)
	if cat == agentErrors.CategoryNone {
		cat = agentErrors.CategoryAuthError // fallback — we know it's auth from stderr
	}

	_ = s.recordAndBroadcast(Event{
		Type: "error",
		Data: map[string]any{
			"message":        cat.ActionMessage(),
			"error_category": string(cat),
			"fatal":          true,
			"retryable":      cat.Retryable(),
		},
	})

	// Kill the agent — no point letting it retry with dead credentials.
	if s.backend != nil {
		s.backend.SendInterrupt()
	}
	s.cancel()
}

// classifySessionError examines accumulated session metrics and agent text
// to determine an error category. Called once at session exit.
func (s *ExternalWSServer) classifySessionError() agentErrors.ErrorCategory {
	s.metricsMu.Lock()
	agentText := s.agentTextBuf.String()
	inputToks := s.totalInputToks
	outputToks := s.totalOutputToks
	outputBytes := s.totalOutputBytes
	s.metricsMu.Unlock()

	// Pattern-based classification from agent output text.
	if cat := agentErrors.Classify(agentText); cat != agentErrors.CategoryNone {
		return cat
	}

	// Startup crash heuristic: zero tokens + tiny output.
	if agentErrors.DetectStartupCrash(inputToks, outputToks, outputBytes) {
		return agentErrors.CategoryStartupCrash
	}

	return agentErrors.CategoryNone
}

// normalizeEvent maps agent-specific data shapes to the standard schema.
// The raw agent data is preserved in a "meta" field for consumers that need it.
func (s *ExternalWSServer) normalizeEvent(event Event) Event {
	raw, _ := event.Data.(map[string]any)
	if raw == nil {
		return event
	}

	switch event.Type {
	case "agent_message":
		switch s.agentType {
		case "claude":
			event.Data = normalizeClaudeAgentMessage(raw)
		case "codex":
			event.Data = normalizeCodexAgentMessage(raw)
		}
	case "tool_use":
		switch s.agentType {
		case "claude":
			event.Data = normalizeClaudeToolUse(raw)
		case "codex":
			event.Data = normalizeCodexToolUse(raw)
		}
	case "tool_result":
		switch s.agentType {
		case "codex":
			event.Data = normalizeCodexToolResult(raw)
		}
	case "result":
		switch s.agentType {
		case "claude":
			event.Data = normalizeClaudeResult(raw)
		case "codex":
			event.Data = normalizeCodexResult(raw)
		}
	}
	return event
}

func (s *ExternalWSServer) exitLoop() {
	waitCh := s.backend.Wait()
	if waitCh == nil {
		return
	}

	select {
	case <-s.ctx.Done():
		return
	case result, ok := <-waitCh:
		if !ok {
			return
		}
		if result.Code != 0 {
			_ = s.recordAndBroadcast(Event{
				Type: "system",
				Data: map[string]any{
					"subtype":      "agent_error",
					"code":         result.Code,
					"error_detail": result.ErrorDetail,
				},
			})
		}
		exitCode := result.Code

		// Classify error from accumulated agent output at session end.
		exitEvent := exitData{
			Code:        result.Code,
			ErrorDetail: result.ErrorDetail,
		}
		if cat := s.classifySessionError(); cat != agentErrors.CategoryNone {
			exitEvent.ErrorCategory = string(cat)
			exitEvent.Retryable = cat.Retryable()
		}

		_ = s.recordAndBroadcast(Event{
			Type:     "exit",
			ExitCode: &exitCode,
			Data:     exitEvent,
		})
		s.startCleanupTimer()
	}
}

// systemEventHeartbeat periodically emits heartbeat system events with current metrics.
func (s *ExternalWSServer) systemEventHeartbeat() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.emitHeartbeat()
		}
	}
}

// emitHeartbeat captures current metrics and broadcasts a heartbeat event.
func (s *ExternalWSServer) emitHeartbeat() {
	// Capture metrics snapshot under lock.
	s.metricsMu.Lock()
	inputToks := s.totalInputToks
	outputToks := s.totalOutputToks
	toolCalls := s.totalToolCalls
	s.metricsMu.Unlock()

	// Calculate uptime in milliseconds.
	s.startMu.RLock()
	startTime := s.startTime
	s.startMu.RUnlock()

	uptimeMs := int64(0)
	if !startTime.IsZero() {
		uptimeMs = time.Since(startTime).Milliseconds()
	}

	_ = s.recordAndBroadcast(Event{
		Type: "system",
		Data: map[string]any{
			"subtype":      "heartbeat",
			"uptime_ms":    uptimeMs,
			"input_tokens": inputToks,
			"output_tokens": outputToks,
			"tool_calls":   toolCalls,
			"agent_running": s.backend != nil && s.backend.Running(),
		},
	})
}

func (s *ExternalWSServer) recordAndBroadcast(event Event) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	// Set timestamp on the event struct itself so it's present in both
	// the replay buffer encoding AND the live WebSocket broadcast.
	// Previously this was only set inside encodeEventLine on a copy.
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	line, _, err := encodeEventLine(s.replay.TotalBytes(), event)
	if err != nil {
		return err
	}

	_, offset := s.replay.WriteOffset(line)
	event.Offset = offset

	var failed []*wsClient
	for _, client := range s.snapshotClients() {
		if err := client.writeJSON(event); err != nil {
			failed = append(failed, client)
		}
	}
	for _, client := range failed {
		s.unregisterClient(client)
		client.close()
	}
	return nil
}

func encodeEventLine(baseOffset int64, event Event) ([]byte, int64, error) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	offset := baseOffset
	for i := 0; i < 6; i++ {
		event.Offset = offset
		line, err := json.Marshal(event)
		if err != nil {
			return nil, 0, err
		}
		next := baseOffset + int64(len(line)+1)
		if next == offset {
			return append(line, '\n'), offset, nil
		}
		offset = next
	}

	event.Offset = offset
	line, err := json.Marshal(event)
	if err != nil {
		return nil, 0, err
	}
	return append(line, '\n'), baseOffset + int64(len(line)+1), nil
}

func (s *ExternalWSServer) registerClient(client *wsClient, hasSince bool, since int64) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	if hasSince {
		data, _ := s.replay.ReadFrom(since)
		for _, event := range decodeReplay(data) {
			if err := client.writeJSON(event); err != nil {
				return err
			}
		}
	}

	s.clientsMu.Lock()
	s.clients[client] = struct{}{}
	s.clientsMu.Unlock()
	s.resetCleanupTimerOnReconnect()
	return nil
}

func decodeReplay(data []byte) []Event {
	lines := bytes.Split(data, []byte{'\n'})
	events := make([]Event, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events
}

func (s *ExternalWSServer) unregisterClient(client *wsClient) {
	s.clientsMu.Lock()
	delete(s.clients, client)
	s.clientsMu.Unlock()
}

func (s *ExternalWSServer) snapshotClients() []*wsClient {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	out := make([]*wsClient, 0, len(s.clients))
	for client := range s.clients {
		out = append(out, client)
	}
	return out
}

func (s *ExternalWSServer) readLoop(client *wsClient) {
	for {
		_, data, err := client.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = client.conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))

		var cmd rawCommand
		if err := json.Unmarshal(data, &cmd); err != nil {
			_ = client.writeJSON(Event{
				Type: "error",
				Data: errorData{Message: "invalid command json: " + err.Error(), Code: http.StatusBadRequest},
			})
			continue
		}

		if err := s.routeCommand(cmd); err != nil {
			_ = client.writeJSON(Event{
				Type: "error",
				Data: errorData{Message: err.Error(), Code: http.StatusBadRequest},
			})
		}
	}
}

func (s *ExternalWSServer) routeCommand(cmd rawCommand) error {
	if s.backend == nil {
		return errors.New("backend unavailable")
	}

	switch cmd.Type {
	case "prompt":
		// Reset stall detection on inbound prompts — the agent is about to work.
		if s.stallDetector != nil {
			s.stallDetector.RecordEvent("prompt")
			s.stallDetector.ClearResult()
		}

		var payload promptCommand
		if err := decodeCommandData(cmd.Data, &payload); err != nil {
			return err
		}
		content, err := requireCommandContent("prompt", payload.Content)
		if err != nil {
			return err
		}
		return s.backend.SendPrompt(content)
	case "interrupt":
		return s.backend.SendInterrupt()
	case "steer":
		var payload steerCommand
		if err := decodeCommandData(cmd.Data, &payload); err != nil {
			return err
		}
		content, err := requireCommandContent("steer", payload.Content)
		if err != nil {
			return err
		}
		return s.backend.SendSteer(content)
	case "context":
		var payload contextCommand
		if err := decodeCommandData(cmd.Data, &payload); err != nil {
			return err
		}
		return s.backend.SendContext(payload.Text, payload.FilePath)
	case "mention":
		var payload mentionCommand
		if err := decodeCommandData(cmd.Data, &payload); err != nil {
			return err
		}
		return s.backend.SendMention(payload.FilePath, payload.LineStart, payload.LineEnd)
	default:
		return errors.New("unknown command type: " + cmd.Type)
	}
}

func (s *ExternalWSServer) sessionID() string {
	if s.backend == nil {
		return ""
	}
	return s.backend.SessionID()
}

func (s *ExternalWSServer) setStartErr(err error) {
	s.startMu.Lock()
	s.startErr = err
	s.startMu.Unlock()
}

func (s *ExternalWSServer) getStartErr() error {
	s.startMu.RLock()
	defer s.startMu.RUnlock()
	return s.startErr
}

func decodeCommandData(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, target)
}

func requireCommandContent(commandType, content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", errors.New(commandType + " content is required")
	}
	return content, nil
}

func parseSince(raw string) (int64, bool, error) {
	if raw == "" {
		return 0, false, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, false, errors.New("since must be non-negative")
	}
	return value, true, nil
}

func (c *wsClient) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.conn.WriteJSON(v)
}

func (c *wsClient) writePing() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.PingMessage, nil)
}

func (c *wsClient) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.writePing(); err != nil {
				return
			}
		}
	}
}

func (c *wsClient) close() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
	})
}

func (s *ExternalWSServer) startCleanupTimer() {
	s.cleanupMu.Lock()
	s.agentExited = true
	s.cleanupMu.Unlock()
	s.resetCleanupTimer()
}

func (s *ExternalWSServer) resetCleanupTimerOnReconnect() {
	s.cleanupMu.Lock()
	shouldReset := s.agentExited
	s.cleanupMu.Unlock()
	if shouldReset {
		s.resetCleanupTimer()
	}
}

func (s *ExternalWSServer) resetCleanupTimer() {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	if !s.agentExited {
		return
	}

	if s.cleanupTimer != nil {
		s.cleanupTimer.Stop()
		s.cleanupTimer = nil
	}

	s.cleanupID++
	id := s.cleanupID
	timeout := s.cleanupTimeout
	s.cleanupTimer = time.AfterFunc(timeout, func() {
		s.fireCleanup(id)
	})
}

func (s *ExternalWSServer) stopCleanupTimer() {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	s.cleanupID++
	if s.cleanupTimer != nil {
		s.cleanupTimer.Stop()
		s.cleanupTimer = nil
	}
}

func (s *ExternalWSServer) fireCleanup(id uint64) {
	s.cleanupMu.Lock()
	if id != s.cleanupID || !s.agentExited {
		s.cleanupMu.Unlock()
		return
	}
	shutdownFn := s.shutdownFn
	s.cleanupTimer = nil
	s.cleanupMu.Unlock()

	if shutdownFn != nil {
		shutdownFn()
	}
}
