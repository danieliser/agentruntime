package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var errAdversarialNoActiveTurn = errors.New("mock backend has no active turn")

func TestExternalWS_MalformedJSONCommandReturnsErrorAndStaysAlive(t *testing.T) {
	backend := newAdversarialMockBackend("sess-malformed")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"prompt","data":`)); err != nil {
		t.Fatalf("write malformed command: %v", err)
	}

	expectErrorEventMessage(t, conn, "invalid command json")

	if err := conn.WriteJSON(Command{
		Type: "prompt",
		Data: map[string]any{"content": "still works"},
	}); err != nil {
		t.Fatalf("write follow-up prompt: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return backend.promptCount() == 1 && backend.lastPrompt() == "still works"
	})
}

func TestExternalWS_UnknownCommandTypeReturnsError(t *testing.T) {
	backend := newAdversarialMockBackend("sess-unknown")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	if err := conn.WriteJSON(Command{
		Type: "definitely-not-real",
		Data: map[string]any{},
	}); err != nil {
		t.Fatalf("write unknown command: %v", err)
	}

	expectErrorEventMessage(t, conn, "unknown command type")
}

func TestExternalWS_EmptyPromptReturnsError(t *testing.T) {
	backend := newAdversarialMockBackend("sess-empty")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	if err := conn.WriteJSON(Command{
		Type: "prompt",
		Data: map[string]any{"content": "   "},
	}); err != nil {
		t.Fatalf("write empty prompt: %v", err)
	}

	expectErrorEventMessage(t, conn, "prompt content is required")
	if got := backend.promptCount(); got != 0 {
		t.Fatalf("promptCount = %d, want 0", got)
	}
}

func TestExternalWS_LargePromptDoesNotCrash(t *testing.T) {
	backend := newAdversarialMockBackend("sess-large")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	largePrompt := strings.Repeat("a", 1<<20)
	if err := conn.WriteJSON(Command{
		Type: "prompt",
		Data: map[string]any{"content": largePrompt},
	}); err != nil {
		t.Fatalf("write 1MB prompt: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return backend.promptCount() == 1
	})
	if got := len(backend.lastPrompt()); got != len(largePrompt) {
		t.Fatalf("large prompt length = %d, want %d", got, len(largePrompt))
	}

	if err := conn.WriteJSON(Command{
		Type: "prompt",
		Data: map[string]any{"content": "follow-up"},
	}); err != nil {
		t.Fatalf("write follow-up prompt after large payload: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return backend.promptCount() == 2 && backend.lastPrompt() == "follow-up"
	})
}

func TestExternalWS_SteerWithoutActiveTurnReturnsError(t *testing.T) {
	backend := newAdversarialMockBackend("sess-steer")
	backend.steerErr = errAdversarialNoActiveTurn

	server, ts := newTestExternalWSServer(t, "codex", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	if err := conn.WriteJSON(Command{
		Type: "steer",
		Data: map[string]any{"content": "change course"},
	}); err != nil {
		t.Fatalf("write steer command: %v", err)
	}

	expectErrorEventMessage(t, conn, errAdversarialNoActiveTurn.Error())
}

func TestExternalWS_InterruptWithoutRunningAgentReturnsError(t *testing.T) {
	backend := newAdversarialMockBackend("sess-interrupt")
	backend.startRunning = false
	backend.requireRunningInterrupt = true

	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	if err := conn.WriteJSON(Command{Type: "interrupt"}); err != nil {
		t.Fatalf("write interrupt command: %v", err)
	}

	expectErrorEventMessage(t, conn, "agent is not running")
}

func TestExternalWS_ContextPathTraversalDoesNotCrash(t *testing.T) {
	backend := newAdversarialMockBackend("sess-context")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	if err := conn.WriteJSON(Command{
		Type: "context",
		Data: map[string]any{
			"text":     "sneaky",
			"filePath": "../../etc/passwd",
		},
	}); err != nil {
		t.Fatalf("write context command: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return backend.contextCount() == 1
	})

	ctx := backend.lastContext()
	if ctx.FilePath != "../../etc/passwd" {
		t.Fatalf("context filePath = %q, want ../../etc/passwd", ctx.FilePath)
	}

	if err := conn.WriteJSON(Command{
		Type: "prompt",
		Data: map[string]any{"content": "after traversal"},
	}); err != nil {
		t.Fatalf("write prompt after traversal context: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return backend.promptCount() == 1 && backend.lastPrompt() == "after traversal"
	})
}

func TestExternalWS_TenClientsReceiveBroadcast(t *testing.T) {
	backend := newAdversarialMockBackend("sess-fanout")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	conns := make([]*websocket.Conn, 0, 10)
	for i := 0; i < 10; i++ {
		conns = append(conns, mustDialWS(t, ts, ""))
	}
	for _, conn := range conns {
		defer conn.Close()
	}

	backend.emit(Event{
		Type: "progress",
		Data: map[string]any{"message": "broadcast"},
	})

	for i, conn := range conns {
		event := readEvent(t, conn)
		if event.Type != "progress" {
			t.Fatalf("client %d event type = %q, want progress", i, event.Type)
		}
	}
}

func TestExternalWS_DisconnectMidStreamDoesNotCrash(t *testing.T) {
	backend := newAdversarialMockBackend("sess-disconnect")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	connA := mustDialWS(t, ts, "")
	connB := mustDialWS(t, ts, "")
	defer connB.Close()

	if err := connA.Close(); err != nil {
		t.Fatalf("close first client: %v", err)
	}

	backend.emit(Event{
		Type: "progress",
		Data: map[string]any{"message": "still streaming"},
	})

	event := readEvent(t, connB)
	if event.Type != "progress" {
		t.Fatalf("second client event type = %q, want progress", event.Type)
	}

	if err := connB.WriteJSON(Command{
		Type: "prompt",
		Data: map[string]any{"content": "after disconnect"},
	}); err != nil {
		t.Fatalf("write prompt from surviving client: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return backend.promptCount() == 1 && backend.lastPrompt() == "after disconnect"
	})
}

func TestExternalWS_HundredRapidPromptsDoNotRace(t *testing.T) {
	backend := newAdversarialMockBackend("sess-burst")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	const clientCount = 5
	const promptsPerClient = 20

	conns := make([]*websocket.Conn, 0, clientCount)
	for i := 0; i < clientCount; i++ {
		conns = append(conns, mustDialWS(t, ts, ""))
	}
	for _, conn := range conns {
		defer conn.Close()
	}

	var wg sync.WaitGroup
	for i, conn := range conns {
		i := i
		conn := conn
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < promptsPerClient; j++ {
				if err := conn.WriteJSON(Command{
					Type: "prompt",
					Data: map[string]any{"content": rapidPromptContent(i, j)},
				}); err != nil {
					t.Errorf("client %d prompt %d write error: %v", i, j, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	waitFor(t, 2*time.Second, func() bool {
		return backend.promptCount() == clientCount*promptsPerClient
	})

	if got := backend.promptCount(); got != clientCount*promptsPerClient {
		t.Fatalf("promptCount = %d, want %d", got, clientCount*promptsPerClient)
	}
}

func TestExternalWS_InvalidSinceReturnsBadRequest(t *testing.T) {
	backend := newAdversarialMockBackend("sess-since")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	resp, err := http.Get(ts.URL + "/ws?since=definitely-not-a-number")
	if err != nil {
		t.Fatalf("GET /ws?since=...: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestExternalWS_HealthReportsAgentRunningTransitions(t *testing.T) {
	backend := newAdversarialMockBackend("sess-health-transitions")
	server, ts := newTestExternalWSServer(t, "claude", backend)
	defer server.Close()

	initial := getHealthResponse(t, ts.URL)
	if initial.AgentRunning {
		t.Fatal("expected agent_running false before websocket connect")
	}

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	waitFor(t, 2*time.Second, func() bool {
		return getHealthResponse(t, ts.URL).AgentRunning
	})

	backend.finish(0)

	waitFor(t, 2*time.Second, func() bool {
		return !getHealthResponse(t, ts.URL).AgentRunning
	})
}

type adversarialMockBackend struct {
	sessionID string
	events    chan Event
	waitCh    chan backendExit

	mu                      sync.RWMutex
	running                 bool
	startRunning            bool
	requireRunningInterrupt bool
	steerErr                error
	prompts                 []string
	contexts                []contextCommand
	mentions                []mentionCommand

	closeOnce sync.Once
}

func newAdversarialMockBackend(sessionID string) *adversarialMockBackend {
	return &adversarialMockBackend{
		sessionID:    sessionID,
		events:       make(chan Event, 256),
		waitCh:       make(chan backendExit, 1),
		startRunning: true,
	}
}

func (b *adversarialMockBackend) Start(ctx context.Context) error {
	b.setRunning(b.startRunning)
	go func() {
		<-ctx.Done()
		b.setRunning(false)
	}()
	return nil
}

func (b *adversarialMockBackend) SendPrompt(content string) error {
	b.mu.Lock()
	b.prompts = append(b.prompts, content)
	b.mu.Unlock()
	return nil
}

func (b *adversarialMockBackend) SendInterrupt() error {
	if b.requireRunningInterrupt && !b.Running() {
		return errors.New("agent is not running")
	}
	return nil
}

func (b *adversarialMockBackend) SendSteer(string) error {
	return b.steerErr
}

func (b *adversarialMockBackend) SendContext(text, filePath string) error {
	b.mu.Lock()
	b.contexts = append(b.contexts, contextCommand{
		Text:     text,
		FilePath: filePath,
	})
	b.mu.Unlock()
	return nil
}

func (b *adversarialMockBackend) SendMention(filePath string, lineStart, lineEnd int) error {
	b.mu.Lock()
	b.mentions = append(b.mentions, mentionCommand{
		FilePath:  filePath,
		LineStart: lineStart,
		LineEnd:   lineEnd,
	})
	b.mu.Unlock()
	return nil
}

func (b *adversarialMockBackend) Events() <-chan Event { return b.events }
func (b *adversarialMockBackend) SessionID() string    { return b.sessionID }

func (b *adversarialMockBackend) Running() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

func (b *adversarialMockBackend) Wait() <-chan backendExit { return b.waitCh }

func (b *adversarialMockBackend) Close() error {
	b.closeOnce.Do(func() {
		b.setRunning(false)
		close(b.waitCh)
	})
	return nil
}

func (b *adversarialMockBackend) emit(event Event) {
	b.events <- event
}

func (b *adversarialMockBackend) finish(code int) {
	b.setRunning(false)
	select {
	case b.waitCh <- backendExit{Code: code}:
	default:
	}
}

func (b *adversarialMockBackend) setRunning(running bool) {
	b.mu.Lock()
	b.running = running
	b.mu.Unlock()
}

func (b *adversarialMockBackend) promptCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.prompts)
}

func (b *adversarialMockBackend) lastPrompt() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.prompts) == 0 {
		return ""
	}
	return b.prompts[len(b.prompts)-1]
}

func (b *adversarialMockBackend) contextCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.contexts)
}

func (b *adversarialMockBackend) lastContext() contextCommand {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.contexts) == 0 {
		return contextCommand{}
	}
	return b.contexts[len(b.contexts)-1]
}

func expectErrorEventMessage(t *testing.T, conn interface{ ReadJSON(any) error }, wantSubstring string) {
	t.Helper()

	var event Event
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read error event: %v", err)
	}
	if event.Type != "error" {
		t.Fatalf("event type = %q, want error", event.Type)
	}

	data, ok := event.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data type = %T, want map[string]any", event.Data)
	}
	message, _ := data["message"].(string)
	if !strings.Contains(message, wantSubstring) {
		t.Fatalf("error message = %q, want substring %q", message, wantSubstring)
	}
}

func getHealthResponse(t *testing.T, baseURL string) healthResponse {
	t.Helper()

	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	var payload healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	return payload
}

func rapidPromptContent(clientID, promptID int) string {
	return strings.Join([]string{"client", strconv.Itoa(clientID), "prompt", strconv.Itoa(promptID)}, "-")
}
