package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/session"
)

// mockSteerableHandle extends mockHandle with SteerableHandle methods.
// It records every sidecar command so tests can verify the bridge
// routed client frames to the correct method with the correct payload.
type mockSteerableHandle struct {
	*mockHandle

	mu       sync.Mutex
	prompts  []string
	steers   []string
	contexts []contextCall
	mentions []mentionCall
	interrupts int
}

type contextCall struct {
	text     string
	filePath string
}

type mentionCall struct {
	filePath  string
	lineStart int
	lineEnd   int
}

func newMockSteerableHandle() *mockSteerableHandle {
	return &mockSteerableHandle{
		mockHandle: newMockHandle(),
	}
}

func (h *mockSteerableHandle) SendPrompt(content string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompts = append(h.prompts, content)
	return nil
}

func (h *mockSteerableHandle) SendInterrupt() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.interrupts++
	return nil
}

func (h *mockSteerableHandle) SendSteer(content string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.steers = append(h.steers, content)
	return nil
}

func (h *mockSteerableHandle) SendContext(text, filePath string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.contexts = append(h.contexts, contextCall{text: text, filePath: filePath})
	return nil
}

func (h *mockSteerableHandle) SendMention(filePath string, lineStart, lineEnd int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mentions = append(h.mentions, mentionCall{filePath: filePath, lineStart: lineStart, lineEnd: lineEnd})
	return nil
}

// bridgeServerWithSteerableHandle creates a bridge test server using a
// mockSteerableHandle so that sendSteerable's type assertion succeeds.
// Returns the test server and a connected WebSocket client.
func bridgeServerWithSteerableHandle(t *testing.T, handle *mockSteerableHandle, replay *session.ReplayBuffer) (*httptest.Server, *websocket.Conn) {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Pass handle (which implements both ProcessHandle and SteerableHandle).
		b := New(conn, handle, replay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Run(ctx, "steerable-test", -1)
	}))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return ts, conn
}

// TestBridge_SteerFrameRouted verifies that a "steer" client frame reaches
// SendSteer on a SteerableHandle with the correct content.
func TestBridge_SteerFrameRouted(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)

	_, conn := bridgeServerWithSteerableHandle(t, h, replay)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{Type: "steer", Data: "focus on error handling"})

	// Wait for the command to be recorded.
	deadline := time.After(3 * time.Second)
	for {
		h.mu.Lock()
		got := len(h.steers)
		h.mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("steer command never reached SendSteer")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.steers) != 1 {
		t.Fatalf("expected 1 steer call, got %d", len(h.steers))
	}
	if h.steers[0] != "focus on error handling" {
		t.Fatalf("expected steer content %q, got %q", "focus on error handling", h.steers[0])
	}

	h.exit(0)
}

// TestBridge_InterruptFrameRouted verifies that an "interrupt" client frame
// calls SendInterrupt on the SteerableHandle.
func TestBridge_InterruptFrameRouted(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)

	_, conn := bridgeServerWithSteerableHandle(t, h, replay)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{Type: "interrupt"})

	deadline := time.After(3 * time.Second)
	for {
		h.mu.Lock()
		got := h.interrupts
		h.mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("interrupt command never reached SendInterrupt")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.interrupts != 1 {
		t.Fatalf("expected 1 interrupt call, got %d", h.interrupts)
	}

	h.exit(0)
}

// TestBridge_ContextFrameRouted verifies that a "context" client frame sends
// the text and file_path through SendContext on the SteerableHandle.
func TestBridge_ContextFrameRouted(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)

	_, conn := bridgeServerWithSteerableHandle(t, h, replay)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{
		Type: "context",
		Context: &ClientFrameContext{
			Text:     "relevant background info",
			FilePath: "/src/main.go",
		},
	})

	deadline := time.After(3 * time.Second)
	for {
		h.mu.Lock()
		got := len(h.contexts)
		h.mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("context command never reached SendContext")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.contexts) != 1 {
		t.Fatalf("expected 1 context call, got %d", len(h.contexts))
	}
	if h.contexts[0].text != "relevant background info" {
		t.Fatalf("expected context text %q, got %q", "relevant background info", h.contexts[0].text)
	}
	if h.contexts[0].filePath != "/src/main.go" {
		t.Fatalf("expected context filePath %q, got %q", "/src/main.go", h.contexts[0].filePath)
	}

	h.exit(0)
}

// TestBridge_MentionFrameRouted verifies that a "mention" client frame sends
// the file path and line range through SendMention on the SteerableHandle.
func TestBridge_MentionFrameRouted(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)

	_, conn := bridgeServerWithSteerableHandle(t, h, replay)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{
		Type: "mention",
		Mention: &ClientFrameMention{
			FilePath:  "/src/handler.go",
			LineStart: 42,
			LineEnd:   58,
		},
	})

	deadline := time.After(3 * time.Second)
	for {
		h.mu.Lock()
		got := len(h.mentions)
		h.mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("mention command never reached SendMention")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.mentions) != 1 {
		t.Fatalf("expected 1 mention call, got %d", len(h.mentions))
	}
	m := h.mentions[0]
	if m.filePath != "/src/handler.go" {
		t.Fatalf("expected mention filePath %q, got %q", "/src/handler.go", m.filePath)
	}
	if m.lineStart != 42 {
		t.Fatalf("expected mention lineStart 42, got %d", m.lineStart)
	}
	if m.lineEnd != 58 {
		t.Fatalf("expected mention lineEnd 58, got %d", m.lineEnd)
	}

	h.exit(0)
}

// TestBridge_SteerOnNonSteerableReturnsError verifies that sending a steer frame
// to a bridge backed by a plain ProcessHandle (not SteerableHandle) returns an
// error frame to the client.
func TestBridge_SteerOnNonSteerableReturnsError(t *testing.T) {
	h := newMockHandle() // plain handle, not steerable
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{Type: "steer", Data: "should fail"})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error frame for steer on non-steerable, got %q", f.Type)
	}
	if !strings.Contains(f.Error, "steerable") && !strings.Contains(f.Error, "sidecar") {
		t.Fatalf("expected error about steerable/sidecar, got %q", f.Error)
	}

	h.exit(0)
}

// TestBridge_InterruptOnNonSteerableReturnsError verifies the same for interrupt.
func TestBridge_InterruptOnNonSteerableReturnsError(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{Type: "interrupt"})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error frame for interrupt on non-steerable, got %q", f.Type)
	}

	h.exit(0)
}

// TestBridge_ContextOnNonSteerableReturnsError verifies the same for context.
func TestBridge_ContextOnNonSteerableReturnsError(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{
		Type:    "context",
		Context: &ClientFrameContext{Text: "should fail"},
	})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error frame for context on non-steerable, got %q", f.Type)
	}

	h.exit(0)
}

// TestBridge_MentionOnNonSteerableReturnsError verifies the same for mention.
func TestBridge_MentionOnNonSteerableReturnsError(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{
		Type:    "mention",
		Mention: &ClientFrameMention{FilePath: "/should/fail.go"},
	})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error frame for mention on non-steerable, got %q", f.Type)
	}

	h.exit(0)
}

// TestBridge_AllCommandTypesSequential sends all four steerable commands in
// sequence and verifies each one was routed to the correct method.
func TestBridge_AllCommandTypesSequential(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)

	_, conn := bridgeServerWithSteerableHandle(t, h, replay)

	readFrame(t, conn) // connected

	// Send all four command types.
	_ = conn.WriteJSON(ClientFrame{Type: "steer", Data: "steer-content"})
	_ = conn.WriteJSON(ClientFrame{Type: "interrupt"})
	_ = conn.WriteJSON(ClientFrame{
		Type:    "context",
		Context: &ClientFrameContext{Text: "ctx-text", FilePath: "/ctx/path"},
	})
	_ = conn.WriteJSON(ClientFrame{
		Type:    "mention",
		Mention: &ClientFrameMention{FilePath: "/mention/path", LineStart: 10, LineEnd: 20},
	})

	// Wait for all commands to arrive.
	deadline := time.After(3 * time.Second)
	for {
		h.mu.Lock()
		allDone := len(h.steers) >= 1 && h.interrupts >= 1 && len(h.contexts) >= 1 && len(h.mentions) >= 1
		h.mu.Unlock()
		if allDone {
			break
		}
		select {
		case <-deadline:
			h.mu.Lock()
			t.Fatalf("timed out waiting for all commands: steers=%d interrupts=%d contexts=%d mentions=%d",
				len(h.steers), h.interrupts, len(h.contexts), len(h.mentions))
			h.mu.Unlock()
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.steers[0] != "steer-content" {
		t.Fatalf("steer content mismatch: %q", h.steers[0])
	}
	if h.contexts[0].text != "ctx-text" || h.contexts[0].filePath != "/ctx/path" {
		t.Fatalf("context mismatch: %+v", h.contexts[0])
	}
	if h.mentions[0].filePath != "/mention/path" || h.mentions[0].lineStart != 10 || h.mentions[0].lineEnd != 20 {
		t.Fatalf("mention mismatch: %+v", h.mentions[0])
	}

	h.exit(0)
}
