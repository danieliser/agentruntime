package main

import (
	"context"
	"encoding/json"
	"errors"
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

func TestExternalWS_HealthEndpointReportsStartError(t *testing.T) {
	backend := newMockBackend("sess-health-error")
	backend.startErr = errors.New("spawn failed")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	event := readEvent(t, conn)
	_ = conn.Close()

	if event.Type != "error" {
		t.Fatalf("expected error event, got %q", event.Type)
	}

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()

	var payload healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if payload.Status != "error" {
		t.Fatalf("expected status error, got %q", payload.Status)
	}
	if payload.ErrorDetail != "spawn failed" {
		t.Fatalf("expected error_detail spawn failed, got %q", payload.ErrorDetail)
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
		t.Fatalf("expected replay data object, got %T", replayed.Data)
	}
	if got, _ := data["text"].(string); got != "second" {
		t.Fatalf("expected replay text second, got %q", got)
	}
}

func TestErrorPropagation_SpawnFailure(t *testing.T) {
	backend := newMockBackend("sess-spawn-failure")
	backend.startErr = errors.New("failed to start agent: permission denied")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	event := readEvent(t, conn)
	if event.Type != "error" {
		t.Fatalf("expected error event, got %q", event.Type)
	}

	data, ok := event.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected error payload map, got %T", event.Data)
	}
	if got, _ := data["message"].(string); got != "failed to start agent: permission denied" {
		t.Fatalf("expected error message, got %q", got)
	}
}

func TestErrorPropagation_AgentCrash(t *testing.T) {
	backend := newMockBackend("sess-agent-crash")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	backend.waitCh <- backendExit{Code: 1, ErrorDetail: "panic: boom\nstderr line"}

	systemEvent := readEvent(t, conn)
	if systemEvent.Type != "system" {
		t.Fatalf("expected system event, got %q", systemEvent.Type)
	}
	systemData, ok := systemEvent.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected system payload map, got %T", systemEvent.Data)
	}
	if got, _ := systemData["subtype"].(string); got != "agent_error" {
		t.Fatalf("expected subtype agent_error, got %q", got)
	}

	exitEvent := readEvent(t, conn)
	if exitEvent.Type != "exit" {
		t.Fatalf("expected exit event, got %q", exitEvent.Type)
	}
	if exitEvent.ExitCode == nil || *exitEvent.ExitCode != 1 {
		t.Fatalf("expected exit_code 1, got %v", exitEvent.ExitCode)
	}
	exitPayload, ok := exitEvent.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected exit payload map, got %T", exitEvent.Data)
	}
	if got, _ := exitPayload["error_detail"].(string); got != "panic: boom\nstderr line" {
		t.Fatalf("expected error_detail in exit payload, got %q", got)
	}
}

func TestSidecar_AutoCleanup_ExitsAfterTimeout(t *testing.T) {
	backend := newMockBackend("sess-cleanup-timeout")
	server, ts, shutdownCh := newAutoCleanupTestServer(t, backend, 75*time.Millisecond)

	conn := mustDialWS(t, ts, "")
	backend.exit(0)

	event := readEvent(t, conn)
	if event.Type != "exit" {
		t.Fatalf("expected exit event, got %q", event.Type)
	}

	select {
	case <-shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cleanup shutdown")
	}

	if _, err := http.Get(ts.URL + "/health"); err == nil {
		t.Fatal("expected sidecar HTTP server to be unavailable after cleanup shutdown")
	}

	_ = conn.Close()
	_ = server.Close()
}

func TestSidecar_AutoCleanup_ResetOnReconnect(t *testing.T) {
	backend := newMockBackend("sess-cleanup-reconnect")
	server, ts, shutdownCh := newAutoCleanupTestServer(t, backend, 120*time.Millisecond)
	defer server.Close()

	conn1 := mustDialWS(t, ts, "")
	backend.exit(0)
	exitEvent := readEvent(t, conn1)
	if exitEvent.Type != "exit" {
		t.Fatalf("expected exit event, got %q", exitEvent.Type)
	}
	_ = conn1.Close()

	time.Sleep(60 * time.Millisecond)

	conn2 := mustDialWS(t, ts, "")
	defer conn2.Close()

	select {
	case <-shutdownCh:
		t.Fatal("cleanup timer did not reset on reconnect")
	case <-time.After(90 * time.Millisecond):
	}

	select {
	case <-shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cleanup after reconnect window")
	}
}

func TestSidecar_AutoCleanup_NoCleanupWhileRunning(t *testing.T) {
	backend := newMockBackend("sess-cleanup-running")
	server, ts, shutdownCh := newAutoCleanupTestServer(t, backend, 75*time.Millisecond)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	select {
	case <-shutdownCh:
		t.Fatal("cleanup timer started before agent exit")
	case <-time.After(175 * time.Millisecond):
	}
}

func newTestExternalWSServer(t *testing.T, agentType string, backend AgentBackend) (*ExternalWSServer, *httptest.Server) {
	t.Helper()

	server := NewExternalWSServer(agentType, backend, StallConfig{})
	ts := httptest.NewServer(server.Routes())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = server.Close() })
	return server, ts
}

func newAutoCleanupTestServer(t *testing.T, backend *mockBackend, timeout time.Duration) (*ExternalWSServer, *httptest.Server, <-chan struct{}) {
	t.Helper()

	server := NewExternalWSServer("claude", backend, StallConfig{})
	server.SetCleanupTimeout(timeout)

	shutdownCh := make(chan struct{}, 1)
	ts := httptest.NewServer(server.Routes())
	server.SetShutdownFunc(func() {
		select {
		case shutdownCh <- struct{}{}:
		default:
		}
		ts.Close()
	})

	t.Cleanup(func() { _ = server.Close() })
	t.Cleanup(ts.Close)
	return server, ts, shutdownCh
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
	waitCh    chan backendExit
	startErr  error

	mu        sync.RWMutex
	running   bool
	prompts   []string
	closeOnce sync.Once
}

func newMockBackend(sessionID string) *mockBackend {
	return &mockBackend{
		sessionID: sessionID,
		events:    make(chan Event, 16),
		waitCh:    make(chan backendExit, 1),
	}
}

func (b *mockBackend) Start(ctx context.Context) error {
	if b.startErr != nil {
		return b.startErr
	}

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
func (b *mockBackend) Wait() <-chan backendExit           { return b.waitCh }
func (b *mockBackend) Close() error {
	b.closeOnce.Do(func() {
		close(b.waitCh)
	})
	return nil
}

func (b *mockBackend) exit(code int) {
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	b.waitCh <- backendExit{Code: code}
}

func (b *mockBackend) emit(event Event) { b.events <- event }

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

func TestHeartbeat_EmittedAtInterval(t *testing.T) {
	// Temporarily reduce heartbeat interval for testing.
	oldInterval := heartbeatInterval
	heartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = oldInterval })

	backend := newMockBackend("sess-heartbeat")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	// Wait for and verify first heartbeat.
	heartbeat1 := readEvent(t, conn)
	if heartbeat1.Type != "system" {
		t.Fatalf("expected system event, got %q", heartbeat1.Type)
	}

	data1, ok := heartbeat1.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected system payload map, got %T", heartbeat1.Data)
	}

	subtype1, ok := data1["subtype"].(string)
	if !ok || subtype1 != "heartbeat" {
		t.Fatalf("expected heartbeat subtype, got %v", data1["subtype"])
	}

	// Wait for second heartbeat to ensure periodic emission.
	heartbeat2 := readEvent(t, conn)
	if heartbeat2.Type != "system" {
		t.Fatalf("expected system event, got %q", heartbeat2.Type)
	}

	data2, ok := heartbeat2.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected system payload map, got %T", heartbeat2.Data)
	}

	subtype2, ok := data2["subtype"].(string)
	if !ok || subtype2 != "heartbeat" {
		t.Fatalf("expected heartbeat subtype, got %v", data2["subtype"])
	}
}

func TestHeartbeat_IncludesMetrics(t *testing.T) {
	// Temporarily reduce heartbeat interval for testing.
	oldInterval := heartbeatInterval
	heartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = oldInterval })

	backend := newMockBackend("sess-heartbeat-metrics")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	// Emit some events to generate metrics.
	backend.emit(Event{
		Type: "tool_use",
		Data: map[string]any{"name": "test_tool"},
	})

	backend.emit(Event{
		Type: "tool_use",
		Data: map[string]any{"name": "test_tool"},
	})

	backend.emit(Event{
		Type: "result",
		Data: map[string]any{
			"usage": map[string]any{
				"input_tokens":  float64(100),
				"output_tokens": float64(50),
			},
		},
	})

	// Read events until we find a heartbeat.
	var heartbeat Event
	found := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// Set a short read timeout to avoid blocking indefinitely
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		event := readEvent(t, conn)
		if event.Type == "system" {
			data, _ := event.Data.(map[string]any)
			if subtype, _ := data["subtype"].(string); subtype == "heartbeat" {
				heartbeat = event
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("timed out waiting for heartbeat")
	}

	data, ok := heartbeat.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected heartbeat payload map, got %T", heartbeat.Data)
	}

	// Check that metrics are present.
	if uptime, ok := data["uptime_ms"].(float64); !ok || uptime < 0 {
		t.Fatalf("expected non-negative uptime_ms, got %v", data["uptime_ms"])
	}

	if inputToks, ok := data["input_tokens"].(float64); !ok || int(inputToks) != 100 {
		t.Fatalf("expected input_tokens 100, got %v", data["input_tokens"])
	}

	if outputToks, ok := data["output_tokens"].(float64); !ok || int(outputToks) != 50 {
		t.Fatalf("expected output_tokens 50, got %v", data["output_tokens"])
	}

	if toolCalls, ok := data["tool_calls"].(float64); !ok || int(toolCalls) != 2 {
		t.Fatalf("expected tool_calls 2, got %v", data["tool_calls"])
	}

	if _, ok := data["agent_running"].(bool); !ok {
		t.Fatalf("expected agent_running bool, got %T", data["agent_running"])
	}
}
