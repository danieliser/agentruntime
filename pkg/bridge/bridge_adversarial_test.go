package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/session"
)

func bridgeServerWithRunDone(t *testing.T, handle *mockHandle, replay *session.ReplayBuffer, sinceOffset int64, sessionID string) (*httptest.Server, *websocket.Conn, <-chan struct{}) {
	t.Helper()

	runDone := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b := New(conn, handle, replay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Run(ctx, sessionID, sinceOffset)
		runDone <- struct{}{}
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
	return ts, conn, runDone
}

func dialBridgeConn(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return conn
}

func waitForRunDone(t *testing.T, runDone <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}

func readBase64Data(t *testing.T, s string) []byte {
	t.Helper()

	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode %q: %v", s, err)
	}
	return data
}

type stdinReadResult struct {
	data []byte
	err  error
}

func readExactFromPipe(r io.Reader, size int) <-chan stdinReadResult {
	done := make(chan stdinReadResult, 1)
	go func() {
		buf := make([]byte, size)
		_, err := io.ReadFull(r, buf)
		done <- stdinReadResult{data: buf, err: err}
	}()
	return done
}

func TestBridge_MalformedJSONStopsBridge(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn, runDone := bridgeServerWithRunDone(t, h, replay, -1, "malformed-session")

	f := readFrame(t, conn)
	if f.Type != "connected" {
		t.Fatalf("expected connected frame, got %q", f.Type)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte("{definitely not json")); err != nil {
		t.Fatalf("write malformed JSON: %v", err)
	}

	waitForRunDone(t, runDone, "bridge after malformed JSON")

	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected connection read to fail after malformed JSON")
	}

	h.exit(0)
}

func TestBridge_MissingTypeFieldReturnsError(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn)

	if err := conn.WriteJSON(struct {
		Data string `json:"data"`
	}{
		Data: "missing type\n",
	}); err != nil {
		t.Fatalf("write frame missing type: %v", err)
	}

	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error frame, got %q", f.Type)
	}
	if !strings.Contains(f.Error, "unknown frame type") {
		t.Fatalf("expected unknown type error, got %q", f.Error)
	}

	h.exit(0)
}

func TestBridge_LargeStdinFrameRouted(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn)

	payload := strings.Repeat("x", (1<<20)+4096)
	stdinDone := readExactFromPipe(h.stdinR, len(payload))

	if err := conn.WriteJSON(ClientFrame{Type: "stdin", Data: payload}); err != nil {
		t.Fatalf("write large stdin frame: %v", err)
	}

	select {
	case result := <-stdinDone:
		if result.err != nil {
			t.Fatalf("read large stdin frame: %v", result.err)
		}
		if !bytes.Equal(result.data, []byte(payload)) {
			t.Fatalf("stdin payload mismatch: got %d bytes, want %d", len(result.data), len(payload))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("large stdin frame never arrived at process")
	}

	h.exit(0)
}

func TestBridge_StdinBeforeConnectedRead(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	payload := "stdin before connected is read\n"
	stdinDone := readExactFromPipe(h.stdinR, len(payload))

	if err := conn.WriteJSON(ClientFrame{Type: "stdin", Data: payload}); err != nil {
		t.Fatalf("write stdin before reading connected: %v", err)
	}

	select {
	case result := <-stdinDone:
		if result.err != nil {
			t.Fatalf("read stdin before connected: %v", result.err)
		}
		if string(result.data) != payload {
			t.Fatalf("expected stdin %q, got %q", payload, string(result.data))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stdin frame sent before connected was not delivered")
	}

	f := readFrame(t, conn)
	if f.Type != "connected" {
		t.Fatalf("expected first unread frame to remain connected, got %q", f.Type)
	}

	h.exit(0)
}

func TestBridge_ExitCode255(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(128)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn)
	go h.exit(255)

	for {
		f := readFrame(t, conn)
		if f.Type == "exit" {
			if f.ExitCode == nil {
				t.Fatal("exit frame had nil exit_code")
			}
			if *f.ExitCode != 255 {
				t.Fatalf("expected exit code 255, got %d", *f.ExitCode)
			}
			return
		}
	}
}

func TestBridge_BinaryStdoutPreservedAsBase64(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn)

	payload := []byte{0xff, 0xfe, 0x00, 'A', '\n'}
	go func() {
		_, _ = h.stdoutW.Write(payload)
		h.exit(0)
	}()

	var got []byte
	for {
		f := readFrame(t, conn)
		if f.Type == "stdout" {
			got = readBase64Data(t, f.Data)
		}
		if f.Type == "exit" {
			break
		}
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("expected binary stdout %v, got %v", payload, got)
	}
}

func TestBridge_RapidReconnectCyclesSameSession(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	runDone := make(chan struct{}, 8)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b := New(conn, h, replay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Run(ctx, "sticky-session", -1)
		runDone <- struct{}{}
	}))
	t.Cleanup(ts.Close)

	for i := 0; i < 8; i++ {
		conn := dialBridgeConn(t, ts)

		f := readFrame(t, conn)
		if f.Type != "connected" {
			t.Fatalf("cycle %d: expected connected frame, got %q", i, f.Type)
		}
		if f.SessionID != "sticky-session" {
			t.Fatalf("cycle %d: expected session_id sticky-session, got %q", i, f.SessionID)
		}

		if err := conn.Close(); err != nil {
			t.Fatalf("cycle %d: close client websocket: %v", i, err)
		}
		waitForRunDone(t, runDone, "reconnect cycle")
	}

	h.exit(0)
}

func TestBridge_RapidStdoutReplayKeepsAllLines(t *testing.T) {
	const lineCount = 10000

	h := newMockHandle()
	var payload strings.Builder
	payload.Grow(lineCount * len("line\n"))
	for i := 0; i < lineCount; i++ {
		payload.WriteString("line\n")
	}
	allOutput := payload.String()

	replay := session.NewReplayBuffer(len(allOutput) + 1024)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn)

	go func() {
		_, _ = h.stdoutW.Write([]byte(allOutput))
		h.exit(0)
	}()

	for {
		f := readFrame(t, conn)
		if f.Type == "exit" {
			break
		}
	}

	data, _ := replay.ReadFrom(0)
	if got := strings.Count(string(data), "\n"); got != lineCount {
		t.Fatalf("expected %d lines in replay buffer, got %d", lineCount, got)
	}
	if string(data) != allOutput {
		t.Fatalf("replay buffer output mismatch: got %d bytes, want %d", len(data), len(allOutput))
	}
}

func TestServerFrame_MarshalOmitsNilExitCode(t *testing.T) {
	data, err := json.Marshal(ServerFrame{Type: "exit"})
	if err != nil {
		t.Fatalf("marshal exit frame: %v", err)
	}
	if strings.Contains(string(data), "exit_code") {
		t.Fatalf("expected exit_code to be omitted, got %s", data)
	}
}

// ---------------------------------------------------------------------------
// Steerable bridge server helpers for adversarial tests.
// Uses mockSteerableHandle from bridge_steerable_test.go.
// ---------------------------------------------------------------------------

// steerableBridgeServerAdv sets up drains and returns a bridge with steerable handle.
func steerableBridgeServerAdv(t *testing.T, handle *mockSteerableHandle, replay *session.ReplayBuffer, sinceOffset int64) (*httptest.Server, *websocket.Conn) {
	t.Helper()

	var drainWg sync.WaitGroup
	drain(handle.stdoutR, replay, &drainWg)
	drain(handle.stderrR, replay, &drainWg)
	go func() {
		drainWg.Wait()
		replay.Close()
	}()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b := New(conn, handle, replay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Run(ctx, "steerable-session", sinceOffset)
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

// steerableBridgeServerWithRunDoneAdv adds completion tracking.
func steerableBridgeServerWithRunDoneAdv(t *testing.T, handle *mockSteerableHandle, replay *session.ReplayBuffer, sinceOffset int64, sessionID string) (*httptest.Server, *websocket.Conn, <-chan struct{}) {
	t.Helper()

	runDone := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b := New(conn, handle, replay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Run(ctx, sessionID, sinceOffset)
		runDone <- struct{}{}
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
	return ts, conn, runDone
}

// ---------------------------------------------------------------------------
// Adversarial tests for sidecar bridge commands (steer, interrupt, context, mention)
// ---------------------------------------------------------------------------

// 1. Send steer frame with empty content — should be accepted without crash.
// The bridge passes content through to the sidecar; validation is the sidecar's
// responsibility. But the bridge must not panic or deadlock on empty data.
func TestBridge_SteerEmptyContent(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := steerableBridgeServerAdv(t, h, replay, -1)

	readFrame(t, conn) // connected

	if err := conn.WriteJSON(ClientFrame{Type: "steer", Data: ""}); err != nil {
		t.Fatalf("write empty steer: %v", err)
	}

	// Send a ping to confirm the bridge is still alive after the empty steer.
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f := readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong after empty steer, got %q", f.Type)
	}

	h.mu.Lock()
	if len(h.steers) != 1 || h.steers[0] != "" {
		t.Fatalf("expected exactly one empty steer call, got %v", h.steers)
	}
	h.mu.Unlock()

	h.exit(0)
}

// 2. Send steer frame with 1MB content — should be handled without crash.
func TestBridge_SteerLargeContent(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := steerableBridgeServerAdv(t, h, replay, -1)

	readFrame(t, conn) // connected

	payload := strings.Repeat("A", 1<<20) // 1MB
	if err := conn.WriteJSON(ClientFrame{Type: "steer", Data: payload}); err != nil {
		t.Fatalf("write large steer: %v", err)
	}

	// Verify it arrived intact.
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f := readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong after large steer, got %q", f.Type)
	}

	h.mu.Lock()
	if len(h.steers) != 1 || len(h.steers[0]) != 1<<20 {
		t.Fatalf("expected 1MB steer call, got %d calls", len(h.steers))
	}
	h.mu.Unlock()

	h.exit(0)
}

// 3. Send interrupt when no session is active (non-steerable handle) —
// should return error frame, not panic.
func TestBridge_InterruptNonSteerableHandle(t *testing.T) {
	h := newMockHandle() // plain handle, NOT steerable
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	if err := conn.WriteJSON(ClientFrame{Type: "interrupt"}); err != nil {
		t.Fatalf("write interrupt: %v", err)
	}

	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error frame for interrupt on non-steerable, got %q", f.Type)
	}
	if !strings.Contains(f.Error, "sidecar commands") {
		t.Fatalf("expected sidecar-commands error, got %q", f.Error)
	}

	// Bridge should still be alive.
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f = readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong, got %q", f.Type)
	}

	h.exit(0)
}

// 4. Send context with path traversal in filePath — should pass through to sidecar
// (the bridge is a transport layer; sanitization is the sidecar's job), but must
// not panic or corrupt state.
func TestBridge_ContextPathTraversal(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := steerableBridgeServerAdv(t, h, replay, -1)

	readFrame(t, conn) // connected

	traversalPath := "../../etc/passwd"
	if err := conn.WriteJSON(ClientFrame{
		Type: "context",
		Context: &ClientFrameContext{
			Text:     "read this file",
			FilePath: traversalPath,
		},
	}); err != nil {
		t.Fatalf("write context: %v", err)
	}

	// Confirm bridge still alive.
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f := readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong, got %q", f.Type)
	}

	h.mu.Lock()
	if len(h.contexts) != 1 {
		t.Fatalf("expected 1 context call, got %d", len(h.contexts))
	}
	got := h.contexts[0]
	h.mu.Unlock()

	if got.filePath != traversalPath {
		t.Fatalf("expected filePath %q, got %q", traversalPath, got.filePath)
	}
	if got.text != "read this file" {
		t.Fatalf("expected text %q, got %q", "read this file", got.text)
	}

	h.exit(0)
}

// 5. Send mention with negative lineStart/lineEnd — should handle gracefully.
func TestBridge_MentionNegativeLines(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := steerableBridgeServerAdv(t, h, replay, -1)

	readFrame(t, conn) // connected

	if err := conn.WriteJSON(ClientFrame{
		Type: "mention",
		Mention: &ClientFrameMention{
			FilePath:  "src/main.go",
			LineStart: -10,
			LineEnd:   -1,
		},
	}); err != nil {
		t.Fatalf("write mention: %v", err)
	}

	// Bridge should not crash.
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f := readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong, got %q", f.Type)
	}

	h.mu.Lock()
	if len(h.mentions) != 1 {
		t.Fatalf("expected 1 mention call, got %d", len(h.mentions))
	}
	got := h.mentions[0]
	h.mu.Unlock()

	if got.lineStart != -10 || got.lineEnd != -1 {
		t.Fatalf("expected lines (-10,-1), got (%d,%d)", got.lineStart, got.lineEnd)
	}

	h.exit(0)
}

// 6. Send 100 rapid-fire steer frames — should not deadlock or crash.
func TestBridge_RapidFireSteer(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := steerableBridgeServerAdv(t, h, replay, -1)

	readFrame(t, conn) // connected

	const count = 100
	for i := 0; i < count; i++ {
		if err := conn.WriteJSON(ClientFrame{Type: "steer", Data: "go"}); err != nil {
			t.Fatalf("steer #%d: %v", i, err)
		}
	}

	// Confirm bridge alive after rapid fire.
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f := readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong after rapid steer, got %q", f.Type)
	}

	// Wait briefly for all calls to be processed, then check count.
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	n := len(h.steers)
	h.mu.Unlock()
	if n != count {
		t.Fatalf("expected %d steer calls, got %d", count, n)
	}

	h.exit(0)
}

// 7. Send unknown frame type — already tested in integration tests, but verify
// the bridge continues processing after returning the error frame.
func TestBridge_UnknownFrameTypeContinues(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := steerableBridgeServerAdv(t, h, replay, -1)

	readFrame(t, conn) // connected

	if err := conn.WriteJSON(ClientFrame{Type: "teleport"}); err != nil {
		t.Fatalf("write unknown: %v", err)
	}
	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error, got %q", f.Type)
	}
	if !strings.Contains(f.Error, "unknown frame type") {
		t.Fatalf("expected unknown frame type in error, got %q", f.Error)
	}

	// Bridge should continue — send a steer and verify it arrives.
	if err := conn.WriteJSON(ClientFrame{Type: "steer", Data: "after unknown"}); err != nil {
		t.Fatalf("write steer: %v", err)
	}
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f = readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong, got %q", f.Type)
	}

	h.mu.Lock()
	if len(h.steers) != 1 || h.steers[0] != "after unknown" {
		t.Fatalf("steer after unknown not delivered: %v", h.steers)
	}
	h.mu.Unlock()

	h.exit(0)
}

// 8. Send malformed JSON in data field — should terminate bridge.
func TestBridge_MalformedJSONDataField(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)
	_, conn, runDone := steerableBridgeServerWithRunDoneAdv(t, h, replay, -1, "malformed-data2")

	f := readFrame(t, conn)
	if f.Type != "connected" {
		t.Fatalf("expected connected, got %q", f.Type)
	}

	// Raw malformed JSON — will fail json.Unmarshal in stdinPump.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"steer","data":INVALID}`)); err != nil {
		t.Fatalf("write malformed: %v", err)
	}

	waitForRunDone(t, runDone, "bridge after malformed JSON data")

	// Connection should be closed.
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected read to fail after malformed JSON")
	}

	h.exit(0)
}

// 9. Disconnect mid-steer — should clean up without goroutine leak.
func TestBridge_DisconnectMidSteer(t *testing.T) {
	h := newMockSteerableHandle()
	replay := session.NewReplayBuffer(256)

	var bridgeReturned atomic.Bool
	bridgeDone := make(chan struct{})

	var drainWg sync.WaitGroup
	drain(h.stdoutR, replay, &drainWg)
	drain(h.stderrR, replay, &drainWg)
	go func() {
		drainWg.Wait()
		replay.Close()
	}()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b := New(conn, h, replay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Run(ctx, "disconnect-mid-steer", -1)
		bridgeReturned.Store(true)
		close(bridgeDone)
	}))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Read connected.
	var sf ServerFrame
	if err := conn.ReadJSON(&sf); err != nil {
		t.Fatalf("read connected: %v", err)
	}

	// Send a steer then immediately close the connection.
	_ = conn.WriteJSON(ClientFrame{Type: "steer", Data: "about to disconnect"})
	conn.Close()

	// Bridge must return cleanly — no goroutine leak.
	select {
	case <-bridgeDone:
		if !bridgeReturned.Load() {
			t.Fatal("bridge done but returned flag not set")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not shut down after mid-steer disconnect")
	}

	h.exit(0)
}

// 10. Send steer to a non-steerable (prompt-mode) session — should return
// an error frame, not panic, and the bridge should keep running.
func TestBridge_SteerOnNonSteerableSession(t *testing.T) {
	h := newMockHandle() // plain handle — simulates prompt-mode / non-interactive
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	// All four sidecar commands should return error frames.
	cmds := []ClientFrame{
		{Type: "steer", Data: "change direction"},
		{Type: "interrupt"},
		{Type: "context", Context: &ClientFrameContext{Text: "extra context"}},
		{Type: "mention", Mention: &ClientFrameMention{FilePath: "foo.go", LineStart: 1, LineEnd: 5}},
	}

	for _, cmd := range cmds {
		if err := conn.WriteJSON(cmd); err != nil {
			t.Fatalf("write %s: %v", cmd.Type, err)
		}
		f := readFrame(t, conn)
		if f.Type != "error" {
			t.Fatalf("expected error for %s on non-steerable, got %q", cmd.Type, f.Type)
		}
		if !strings.Contains(f.Error, "sidecar commands") {
			t.Fatalf("expected sidecar-commands error for %s, got %q", cmd.Type, f.Error)
		}
	}

	// Bridge should still be alive.
	if err := conn.WriteJSON(ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f := readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong, got %q", f.Type)
	}

	h.exit(0)
}

// TestBridge_SimultaneousStdoutAndStderrBothCaptured verifies that both stdout
// and stderr content arrives via the replay buffer. Since the replay buffer
// merges both streams, all data arrives as "stdout" frames.
func TestBridge_SimultaneousStdoutAndStderrBothCaptured(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(256)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn)

	start := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(2)

	go func() {
		defer writers.Done()
		<-start
		_, _ = h.stdoutW.Write([]byte("stdout payload\n"))
	}()
	go func() {
		defer writers.Done()
		<-start
		_, _ = h.stderrW.Write([]byte("stderr payload\n"))
	}()
	go func() {
		writers.Wait()
		h.exit(0)
	}()

	close(start)

	var gotStdout bool
	var gotStderr bool
	for {
		f := readFrame(t, conn)
		switch f.Type {
		case "stdout":
			if strings.Contains(f.Data, "stdout payload") {
				gotStdout = true
			}
			if strings.Contains(f.Data, "stderr payload") {
				gotStderr = true
			}
		case "exit":
			if !gotStdout {
				t.Fatal("never received stdout payload")
			}
			if !gotStderr {
				t.Fatal("never received stderr payload")
			}
			return
		}
	}
}
