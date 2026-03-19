package bridge

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func decodeBase64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode %q: %v", s, err)
	}
	return string(b)
}

// mockHandle implements runtime.ProcessHandle with in-process pipes.
// This lets us drive bridge behaviour deterministically without spawning
// real processes — critical for testing edge cases like stderr output,
// non-zero exits, stdin propagation, and simultaneous read/write.
type mockHandle struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter
	done    chan runtime.ExitResult
	pid     int
}

func newMockHandle() *mockHandle {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &mockHandle{
		stdinR:  stdinR,
		stdinW:  stdinW,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		stderrR: stderrR,
		stderrW: stderrW,
		done:    make(chan runtime.ExitResult, 1),
		pid:     42,
	}
}

func (h *mockHandle) Stdin() io.WriteCloser           { return h.stdinW }
func (h *mockHandle) Stdout() io.ReadCloser           { return h.stdoutR }
func (h *mockHandle) Stderr() io.ReadCloser           { return h.stderrR }
func (h *mockHandle) Wait() <-chan runtime.ExitResult { return h.done }
func (h *mockHandle) Kill() error                     { h.exit(137); return nil }
func (h *mockHandle) PID() int                        { return h.pid }
func (h *mockHandle) RecoveryInfo() *runtime.RecoveryInfo {
	return nil
}

func (h *mockHandle) exit(code int) {
	h.stdoutW.Close()
	h.stderrW.Close()
	select {
	case h.done <- runtime.ExitResult{Code: code}:
	default:
	}
}

// drain reads from r and writes to replay, same as the production handler.
func drain(r io.ReadCloser, replay *session.ReplayBuffer, wg *sync.WaitGroup) {
	if r == nil {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				replay.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
}

// bridgeServer wraps a mock handle in a real httptest server so we can
// dial it via WebSocket exactly as a production client would.
// It starts drain goroutines that read from the mock's pipes and write to
// the replay buffer — mirroring what the production handler does.
func bridgeServer(t *testing.T, handle *mockHandle, replay *session.ReplayBuffer, sinceOffset int64) (*httptest.Server, *websocket.Conn) {
	t.Helper()

	// Start drains: pipe → replay (same as production handler).
	// Drains exit on pipe EOF (when exit() closes stdoutW/stderrW).
	var drainWg sync.WaitGroup
	drain(handle.stdoutR, replay, &drainWg)
	drain(handle.stderrR, replay, &drainWg)

	// Close replay after all drains finish (pipe EOFs from exit()).
	// Do NOT consume from handle.done — the bridge needs that for the exit code.
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
		b := New(conn, handle, replay, "", "")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Run(ctx, "test-session", sinceOffset)
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

func readFrame(t *testing.T, conn *websocket.Conn) ServerFrame {
	t.Helper()
	var f ServerFrame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

// TestBridge_ConnectedFrameFirst verifies that the first frame is always "connected"
// and contains the expected session ID. This is the contract clients depend on to
// know they're wired up.
func TestBridge_ConnectedFrameFirst(t *testing.T) {
	h := newMockHandle()
	go h.exit(0)

	replay := session.NewReplayBuffer(1024)
	_, conn := bridgeServer(t, h, replay, -1)

	f := readFrame(t, conn)
	if f.Type != "connected" {
		t.Fatalf("expected first frame 'connected', got %q", f.Type)
	}
	if f.SessionID != "test-session" {
		t.Fatalf("expected session_id 'test-session', got %q", f.SessionID)
	}
}

// TestBridge_StdoutFrameDelivered verifies that data written to the process stdout
// arrives as a "stdout" frame on the WebSocket before the exit frame.
func TestBridge_StdoutFrameDelivered(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(1024)
	_, conn := bridgeServer(t, h, replay, -1)

	// Drain connected — confirms bridge is fully running before we produce output.
	readFrame(t, conn)

	// Write to stdout then exit. Small sleep ensures the readPump goroutine
	// has its scanner positioned before we write, avoiding a race on first read.
	go func() {
		time.Sleep(10 * time.Millisecond)
		h.stdoutW.Write([]byte("agent output line\n"))
		h.exit(0)
	}()

	// Collect until exit.
	var gotLine bool
	for {
		f := readFrame(t, conn)
		if f.Type == "stdout" && strings.Contains(f.Data, "agent output line") {
			gotLine = true
		}
		if f.Type == "exit" {
			break
		}
	}
	if !gotLine {
		t.Fatal("never received stdout frame with expected content")
	}
}

// TestBridge_StderrCapturedViaReplay verifies that stderr output is captured
// to the replay buffer and arrives via stdout frames (the replay buffer
// merges both streams — stream identity is not preserved).
func TestBridge_StderrCapturedViaReplay(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(1024)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	go func() {
		time.Sleep(10 * time.Millisecond)
		h.stderrW.Write([]byte("error output\n"))
		h.exit(0)
	}()

	var gotOutput bool
	for {
		f := readFrame(t, conn)
		if f.Type == "stdout" && strings.Contains(f.Data, "error output") {
			gotOutput = true
		}
		if f.Type == "exit" {
			break
		}
	}
	if !gotOutput {
		t.Fatal("never received stderr content in stdout frame")
	}
}

// TestBridge_ExitFrameCode verifies that the exit code in the "exit" frame matches
// what the process returned. Callers use this to determine success/failure.
func TestBridge_ExitFrameCode(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"zero", 0},
		{"one", 1},
		{"forty_two", 42},
		{"sigkill", 137},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := newMockHandle()
			replay := session.NewReplayBuffer(128)
			_, conn := bridgeServer(t, h, replay, -1)

			readFrame(t, conn) // connected
			go h.exit(tc.code)

			for {
				f := readFrame(t, conn)
				if f.Type == "exit" {
					if f.ExitCode == nil {
						t.Fatal("exit frame has nil exit_code")
					}
					if *f.ExitCode != tc.code {
						t.Fatalf("expected exit code %d, got %d", tc.code, *f.ExitCode)
					}
					return
				}
			}
		})
	}
}

// TestBridge_StdinRoutedToProcess verifies that a "stdin" ClientFrame written to the
// WebSocket reaches the process stdin pipe. The mockHandle lets us read stdin directly.
func TestBridge_StdinRoutedToProcess(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(1024)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	// Send stdin frame.
	_ = conn.WriteJSON(ClientFrame{Type: "stdin", Data: "steered command\n"})

	// Read from the mock handle's stdin pipe — must arrive.
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := h.stdinR.Read(buf)
		done <- string(buf[:n])
	}()

	select {
	case got := <-done:
		if !strings.Contains(got, "steered command") {
			t.Fatalf("expected 'steered command' in stdin, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stdin frame never arrived at process")
	}

	// Clean up.
	go h.exit(0)
}

// TestBridge_ReplayFrameBeforeConnected verifies that when sinceOffset=0, a replay
// frame is sent BEFORE the connected frame. Reconnecting clients must see buffered
// output before the stream continues — order matters.
func TestBridge_ReplayFrameBeforeConnected(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(1024)
	replay.Write([]byte("buffered output\n"))

	go h.exit(0)

	_, conn := bridgeServer(t, h, replay, 0) // sinceOffset=0 → replay everything

	frames := make([]ServerFrame, 0, 4)
	for {
		f := readFrame(t, conn)
		frames = append(frames, f)
		if f.Type == "connected" || len(frames) >= 2 {
			break
		}
	}

	if len(frames) < 2 {
		t.Fatalf("expected at least 2 frames (replay + connected), got %d", len(frames))
	}
	if frames[0].Type != "replay" {
		t.Fatalf("expected first frame 'replay', got %q", frames[0].Type)
	}
	// Replay data is base64-encoded — decode and check content.
	decoded := decodeBase64(t, frames[0].Data)
	if !strings.Contains(decoded, "buffered output") {
		t.Fatalf("replay frame missing buffered content, got %q", decoded)
	}
	if frames[1].Type != "connected" {
		t.Fatalf("expected second frame 'connected', got %q", frames[1].Type)
	}
}

// TestBridge_NoReplayWhenSinceNegative verifies that sinceOffset=-1 suppresses
// the replay frame entirely — fresh connections shouldn't see historical output
// unless they explicitly request it.
func TestBridge_NoReplayWhenSinceNegative(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(1024)
	replay.Write([]byte("old output\n"))

	go h.exit(0)

	_, conn := bridgeServer(t, h, replay, -1) // no replay

	f := readFrame(t, conn)
	if f.Type == "replay" {
		t.Fatal("expected no replay frame for sinceOffset=-1, but got one")
	}
	if f.Type != "connected" {
		t.Fatalf("expected first frame 'connected', got %q", f.Type)
	}
}

// TestBridge_ReplayPartialOffset verifies that sinceOffset=N replays only bytes
// from offset N onward. A client that missed 7 bytes of a 14-byte buffer should
// only receive the last 7 bytes in the replay frame.
func TestBridge_ReplayPartialOffset(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(1024)
	replay.Write([]byte("first-\n")) // 7 bytes, offset 0–6
	replay.Write([]byte("second\n")) // 7 bytes, offset 7–13

	go h.exit(0)

	_, conn := bridgeServer(t, h, replay, 7) // reconnect from offset 7

	f := readFrame(t, conn)
	if f.Type != "replay" {
		t.Fatalf("expected replay frame, got %q", f.Type)
	}
	// Replay data is base64-encoded — decode to check content.
	decoded := decodeBase64(t, f.Data)
	if strings.Contains(decoded, "first-") {
		t.Fatalf("replay should not include bytes before offset 7, but got %q", decoded)
	}
	if !strings.Contains(decoded, "second") {
		t.Fatalf("replay should include bytes from offset 7, got %q", decoded)
	}
}

// TestBridge_OutputWrittenToReplayBuffer verifies that stdout written through the
// drain pipeline is accumulated in the replay buffer. This is the durability guarantee:
// output survives WS disconnections as long as the buffer hasn't wrapped.
func TestBridge_OutputWrittenToReplayBuffer(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(1024)

	// Start drains like production handler does.
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
		b := New(conn, h, replay, "", "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.Run(ctx, "sid", -1)
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Write output then exit.
	go func() {
		h.stdoutW.Write([]byte("durable output\n"))
		h.exit(0)
	}()

	// Drain frames.
	for {
		var f ServerFrame
		if err := conn.ReadJSON(&f); err != nil {
			break
		}
		if f.Type == "exit" {
			break
		}
	}
	conn.Close()

	// Replay buffer must contain the output.
	data, _ := replay.ReadFrom(0)
	if !strings.Contains(string(data), "durable output") {
		t.Fatalf("replay buffer missing output, got %q", string(data))
	}
}

// TestBridge_PingPongAppLevel verifies the application-layer ping/pong.
// This is distinct from the WebSocket protocol ping/pong — it keeps
// higher-level heartbeats working through proxies that may not forward WS pings.
func TestBridge_PingPongAppLevel(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(128)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{Type: "ping"})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f := readFrame(t, conn)
	if f.Type != "pong" {
		t.Fatalf("expected pong, got %q", f.Type)
	}

	go h.exit(0)
}

// TestBridge_UnknownClientFrame verifies the server sends an error frame for
// unrecognised frame types. Silent drops would make debugging impossible.
func TestBridge_UnknownClientFrame(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(128)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	_ = conn.WriteJSON(ClientFrame{Type: "absolutely-not-a-type"})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f := readFrame(t, conn)
	if f.Type != "error" {
		t.Fatalf("expected error frame, got %q", f.Type)
	}
	if f.Error == "" {
		t.Fatal("expected non-empty error message in error frame")
	}

	go h.exit(0)
}

// TestBridge_MultipleStdoutLines verifies that all lines from a multi-line output
// are delivered as stdout frames. The bridge uses a line scanner so each \n
// boundary produces a frame — but rapid writes may batch into fewer scanner reads.
// We verify total line count across all frames, not frame count.
func TestBridge_MultipleStdoutLines(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(4096)
	_, conn := bridgeServer(t, h, replay, -1)

	readFrame(t, conn) // connected

	go func() {
		time.Sleep(10 * time.Millisecond)
		for i := 0; i < 5; i++ {
			h.stdoutW.Write([]byte("line\n"))
			time.Sleep(5 * time.Millisecond) // let scanner pick each line separately
		}
		h.exit(0)
	}()

	var totalLines int
	for {
		f := readFrame(t, conn)
		if f.Type == "stdout" {
			// Count newlines in the frame data (scanner may batch).
			totalLines += strings.Count(f.Data, "\n")
		}
		if f.Type == "exit" {
			break
		}
	}
	if totalLines < 5 {
		t.Fatalf("expected at least 5 newlines across stdout frames, got %d", totalLines)
	}
}

// TestBridge_WSDischconnectCancelsProcess verifies that when the WebSocket client
// disconnects, the bridge's context is cancelled — stopping the read pumps cleanly.
// This is the "client navigates away" scenario.
func TestBridge_WSDisconnectCancelsCleanly(t *testing.T) {
	h := newMockHandle()
	replay := session.NewReplayBuffer(128)
	bridgeDone := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b := New(conn, h, replay, "", "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.Run(ctx, "sid", -1)
		close(bridgeDone)
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Drain connected.
	var f ServerFrame
	_ = conn.ReadJSON(&f)

	// Client disconnects abruptly.
	conn.Close()

	// Bridge goroutine must exit — if it doesn't, the daemon leaks goroutines.
	select {
	case <-bridgeDone:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not shut down after WS disconnect")
	}

	// Clean up mock process.
	h.exit(0)
}
