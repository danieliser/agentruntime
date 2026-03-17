package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const writeTimeout = 5 * time.Second

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

type Event struct {
	Type   string         `json:"type"`
	Data   map[string]any `json:"data,omitempty"`
	Offset int64          `json:"offset,omitempty"`
}

type Command struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

type healthResponse struct {
	Status       string `json:"status"`
	AgentRunning bool   `json:"agent_running"`
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id,omitempty"`
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
	Close() error
}

type replayEntry struct {
	offset int64
	event  Event
}

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *wsConn) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.conn.WriteJSON(v)
}

type ExternalWSServer struct {
	agentType string
	backend   AgentBackend

	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	startErr  error

	mu      sync.RWMutex
	conns   map[*websocket.Conn]*wsConn
	replay  []replayEntry
	nextOff int64
}

func NewExternalWSServer(agentType string, backend AgentBackend) *ExternalWSServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &ExternalWSServer{
		agentType: agentType,
		backend:   backend,
		ctx:       ctx,
		cancel:    cancel,
		conns:     make(map[*websocket.Conn]*wsConn),
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

	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
		delete(s.conns, conn)
	}
	s.mu.Unlock()

	if s.backend != nil {
		return s.backend.Close()
	}
	return nil
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
	since, err := parseSince(r.URL.Query().Get("since"))
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

	client := &wsConn{conn: conn}
	s.addConn(conn, client)
	defer s.removeConn(conn)
	defer conn.Close()

	for _, event := range s.replayAfter(since) {
		if err := client.writeJSON(event); err != nil {
			return
		}
	}

	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	for {
		var cmd Command
		if err := conn.ReadJSON(&cmd); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		if err := s.routeCommand(cmd); err != nil {
			_ = client.writeJSON(Event{
				Type: "error",
				Data: map[string]any{"message": err.Error()},
			})
		}
	}
}

func (s *ExternalWSServer) ensureStarted() error {
	s.startOnce.Do(func() {
		if s.backend == nil {
			s.startErr = errors.New("backend is nil")
			return
		}

		s.startErr = s.backend.Start(s.ctx)
		if s.startErr != nil {
			return
		}

		go s.eventLoop()
	})
	return s.startErr
}

func (s *ExternalWSServer) eventLoop() {
	if s.backend == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-s.backend.Events():
			if !ok {
				return
			}
			s.recordAndBroadcast(event)
		}
	}
}

func (s *ExternalWSServer) recordAndBroadcast(event Event) {
	s.mu.Lock()
	s.nextOff++
	event.Offset = s.nextOff
	s.replay = append(s.replay, replayEntry{
		offset: event.Offset,
		event:  event,
	})

	clients := make([]*wsConn, 0, len(s.conns))
	for _, client := range s.conns {
		clients = append(clients, client)
	}
	s.mu.Unlock()

	for _, client := range clients {
		_ = client.writeJSON(event)
	}
}

func (s *ExternalWSServer) replayAfter(offset int64) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]Event, 0, len(s.replay))
	for _, entry := range s.replay {
		if entry.offset > offset {
			events = append(events, entry.event)
		}
	}
	return events
}

func (s *ExternalWSServer) addConn(raw *websocket.Conn, client *wsConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[raw] = client
}

func (s *ExternalWSServer) removeConn(raw *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, raw)
}

func (s *ExternalWSServer) routeCommand(cmd Command) error {
	if s.backend == nil {
		return errors.New("backend unavailable")
	}

	switch cmd.Type {
	case "prompt":
		return s.backend.SendPrompt(stringValue(cmd.Data["content"]))
	case "interrupt":
		return s.backend.SendInterrupt()
	case "steer":
		return s.backend.SendSteer(stringValue(cmd.Data["content"]))
	case "context":
		return s.backend.SendContext(stringValue(cmd.Data["text"]), stringValue(cmd.Data["filePath"]))
	case "mention":
		return s.backend.SendMention(
			stringValue(cmd.Data["filePath"]),
			intValue(cmd.Data["lineStart"]),
			intValue(cmd.Data["lineEnd"]),
		)
	case "ping":
		return nil
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

func parseSince(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("since must be non-negative")
	}
	return value, nil
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
