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

func TestBridge_SimultaneousStdoutAndStderrKeepFrameTypes(t *testing.T) {
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
		case "stderr":
			if strings.Contains(f.Data, "stderr payload") {
				gotStderr = true
			}
		case "exit":
			if !gotStdout {
				t.Fatal("never received stdout payload as a stdout frame")
			}
			if !gotStderr {
				t.Fatal("never received stderr payload as a stderr frame")
			}
			return
		}
	}
}
