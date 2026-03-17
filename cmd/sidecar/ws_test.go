package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestExternalWS_HealthEndpoint(t *testing.T) {
	backend := newMockBackend("sess-health")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var payload healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if payload.Status != "ok" {
		t.Fatalf("expected status ok, got %q", payload.Status)
	}
	if payload.AgentRunning {
		t.Fatal("expected agent_running false before start")
	}
	if payload.AgentType != "claude" {
		t.Fatalf("expected agent_type claude, got %q", payload.AgentType)
	}
	if payload.SessionID != "sess-health" {
		t.Fatalf("expected session_id sess-health, got %q", payload.SessionID)
	}
}

func TestExternalWS_EventBroadcast(t *testing.T) {
	backend := newMockBackend("sess-broadcast")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	connA := mustDialWS(t, ts, "")
	defer connA.Close()
	connB := mustDialWS(t, ts, "")
	defer connB.Close()

	backend.emit(Event{
		Type: "progress",
		Data: map[string]any{
			"status":  "running",
			"message": "Reading files",
		},
	})

	frameA := readEvent(t, connA)
	frameB := readEvent(t, connB)

	if frameA.Type != "progress" || frameB.Type != "progress" {
		t.Fatalf("expected progress events, got %q and %q", frameA.Type, frameB.Type)
	}
	if frameA.Offset == 0 || frameB.Offset == 0 {
		t.Fatal("expected non-zero replay offsets")
	}
}

func TestExternalWS_PromptRouting(t *testing.T) {
	backend := newMockBackend("sess-prompt")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	if err := conn.WriteJSON(Command{
		Type: "prompt",
		Data: map[string]any{"content": "fix the bug"},
	}); err != nil {
		t.Fatalf("write prompt command: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for backend prompt")
		default:
			if backend.lastPrompt() == "fix the bug" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestExternalWS_ReplayOnReconnect(t *testing.T) {
	backend := newMockBackend("sess-replay")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn1 := mustDialWS(t, ts, "")

	backend.emit(Event{
		Type: "agent_message",
		Data: map[string]any{"text": "first"},
	})
	first := readEvent(t, conn1)

	backend.emit(Event{
		Type: "agent_message",
		Data: map[string]any{"text": "second"},
	})
	second := readEvent(t, conn1)
	conn1.Close()

	conn2 := mustDialWS(t, ts, "?since="+strconv.FormatInt(first.Offset, 10))
	defer conn2.Close()

	replayed := readEvent(t, conn2)
	if replayed.Type != "agent_message" {
		t.Fatalf("expected replayed agent_message, got %q", replayed.Type)
	}
	if replayed.Offset != second.Offset {
		t.Fatalf("expected replay offset %d, got %d", second.Offset, replayed.Offset)
	}

	data, ok := replayed.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected object data, got %T", replayed.Data)
	}
	if got, _ := data["text"].(string); got != "second" {
		t.Fatalf("expected replay text second, got %q", got)
	}
}

func newTestExternalWSServer(t *testing.T, agentType string, backend *mockBackend) (*ExternalWSServer, *httptest.Server) {
	t.Helper()

	server := NewExternalWSServer(agentType, backend)
	ts := httptest.NewServer(server.Routes())
	t.Cleanup(ts.Close)
	t.Cleanup(server.Close)
	return server, ts
}

func mustDialWS(t *testing.T, ts *httptest.Server, suffix string) *websocket.Conn {
	t.Helper()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws" + suffix
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func readEvent(t *testing.T, conn *websocket.Conn) Event {
	t.Helper()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var event Event
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read event: %v", err)
	}
	return event
}

type mockBackend struct {
	sessionID string
	events    chan Event
	waitCh    chan int

	mu      sync.RWMutex
	running bool
	prompts []string
}

func newMockBackend(sessionID string) *mockBackend {
	return &mockBackend{
		sessionID: sessionID,
		events:    make(chan Event, 16),
		waitCh:    make(chan int, 1),
	}
}

func (b *mockBackend) Start(ctx context.Context) error {
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
	}()
	return nil
}

func (b *mockBackend) SendPrompt(content string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prompts = append(b.prompts, content)
	return nil
}

func (b *mockBackend) SendInterrupt() error               { return nil }
func (b *mockBackend) SendSteer(string) error             { return nil }
func (b *mockBackend) SendContext(string, string) error   { return nil }
func (b *mockBackend) SendMention(string, int, int) error { return nil }
func (b *mockBackend) Events() <-chan Event               { return b.events }
func (b *mockBackend) SessionID() string                  { return b.sessionID }
func (b *mockBackend) Wait() <-chan int                   { return b.waitCh }
func (b *mockBackend) emit(event Event)                   { b.events <- event }

func (b *mockBackend) Running() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

func (b *mockBackend) lastPrompt() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.prompts) == 0 {
		return ""
	}
	return b.prompts[len(b.prompts)-1]
}
