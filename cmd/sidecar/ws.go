package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/session"
)

const (
	replayBufferSize = 1 << 20
	writeTimeout     = 5 * time.Second
	pingInterval     = 30 * time.Second
	pongTimeout      = 10 * time.Second
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

type Event struct {
	Type      string `json:"type"`
	Data      any    `json:"data"`
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
	Wait() <-chan int
}

type healthResponse struct {
	Status       string `json:"status"`
	AgentRunning bool   `json:"agent_running"`
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id"`
}

type ExternalWSServer struct {
	agentType string
	backend   AgentBackend
	replay    *session.ReplayBuffer

	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	startErr  error

	appendMu sync.Mutex

	clientsMu sync.RWMutex
	clients   map[*wsClient]struct{}
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
	Code int `json:"code"`
}

type rawCommand struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func NewExternalWSServer(agentType string, backend AgentBackend) *ExternalWSServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &ExternalWSServer{
		agentType: agentType,
		backend:   backend,
		replay:    session.NewReplayBuffer(replayBufferSize),
		ctx:       ctx,
		cancel:    cancel,
		clients:   make(map[*wsClient]struct{}),
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

func (s *ExternalWSServer) Close() error {
	s.cancel()
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:       "ok",
		AgentRunning: s.backend != nil && s.backend.Running(),
		AgentType:    s.agentType,
		SessionID:    s.sessionID(),
	})
}

func (s *ExternalWSServer) handleWS(w http.ResponseWriter, r *http.Request) {
	since, hasSince, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		http.Error(w, "invalid since", http.StatusBadRequest)
		return
	}

	if err := s.ensureStarted(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := &wsClient{conn: conn}
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
			s.startErr = errors.New("backend unavailable")
			return
		}

		if err := s.backend.Start(s.ctx); err != nil {
			s.startErr = err
			return
		}

		go s.eventLoop()
		go s.exitLoop()
	})
	return s.startErr
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
			event = s.normalizeEvent(event)
			_ = s.recordAndBroadcast(event)
		}
	}
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
	case code, ok := <-waitCh:
		if !ok {
			return
		}
		_ = s.recordAndBroadcast(Event{
			Type: "exit",
			Data: exitData{Code: code},
		})
	}
}

func (s *ExternalWSServer) recordAndBroadcast(event Event) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

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
		var cmd rawCommand
		if err := client.conn.ReadJSON(&cmd); err != nil {
			return
		}
		_ = client.conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))

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
		var payload promptCommand
		if err := decodeCommandData(cmd.Data, &payload); err != nil {
			return err
		}
		return s.backend.SendPrompt(payload.Content)
	case "interrupt":
		return s.backend.SendInterrupt()
	case "steer":
		var payload steerCommand
		if err := decodeCommandData(cmd.Data, &payload); err != nil {
			return err
		}
		return s.backend.SendSteer(payload.Content)
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

func decodeCommandData(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, target)
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
