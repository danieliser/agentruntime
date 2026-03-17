package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSHandle_StdoutRouting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			t.Fatalf("expected /ws path, got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("since"); got != "0" {
			t.Fatalf("expected since=0, got %q", got)
		}

		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(wsServerFrame{Type: "connected"}); err != nil {
			t.Fatalf("write connected: %v", err)
		}
		if err := conn.WriteJSON(wsServerFrame{Type: "stdout", Data: json.RawMessage(`"hello from sidecar"`)}); err != nil {
			t.Fatalf("write stdout: %v", err)
		}
		code := 0
		if err := conn.WriteJSON(wsServerFrame{Type: "exit", ExitCode: &code}); err != nil {
			t.Fatalf("write exit: %v", err)
		}
	}))
	defer server.Close()

	handle := dialWSHandle(t, server, "container-123", 0)

	got, err := io.ReadAll(handle.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if string(got) != "hello from sidecar" {
		t.Fatalf("expected stdout routing, got %q", string(got))
	}
}

func TestWSHandle_StdinRouting(t *testing.T) {
	received := make(chan wsClientFrame, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(wsServerFrame{Type: "connected"}); err != nil {
			t.Fatalf("write connected: %v", err)
		}

		var frame wsClientFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read stdin frame: %v", err)
		}
		received <- frame

		code := 0
		if err := conn.WriteJSON(wsServerFrame{Type: "exit", ExitCode: &code}); err != nil {
			t.Fatalf("write exit: %v", err)
		}
	}))
	defer server.Close()

	handle := dialWSHandle(t, server, "container-stdin", -1)

	if _, err := handle.Stdin().Write([]byte("typed input")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	select {
	case frame := <-received:
		if frame.Type != "stdin" {
			t.Fatalf("expected stdin frame type, got %q", frame.Type)
		}
		if got := wsClientFrameStringData(t, frame); got != "typed input" {
			t.Fatalf("expected stdin payload %q, got %q", "typed input", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stdin frame")
	}
}

func TestWSHandle_ExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(wsServerFrame{Type: "connected"}); err != nil {
			t.Fatalf("write connected: %v", err)
		}
		code := 17
		if err := conn.WriteJSON(wsServerFrame{Type: "exit", ExitCode: &code}); err != nil {
			t.Fatalf("write exit: %v", err)
		}
	}))
	defer server.Close()

	handle := dialWSHandle(t, server, "container-exit", -1)

	select {
	case result := <-handle.Wait():
		if result.Err != nil {
			t.Fatalf("wait returned error: %v", result.Err)
		}
		if result.Code != 17 {
			t.Fatalf("expected exit code 17, got %d", result.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for exit result")
	}
}

func TestErrorPropagation_WSDisconnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		if err := conn.WriteJSON(wsServerFrame{
			Type: "error",
			Data: json.RawMessage(`{"message":"sidecar lost upstream connection"}`),
		}); err != nil {
			t.Fatalf("write error frame: %v", err)
		}
		// Small delay to ensure the client reads the error frame before the close
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close()
	}))
	defer server.Close()

	handle := dialWSHandle(t, server, "container-disconnect", -1)

	// Drain stdout so writeEvent doesn't block on the pipe
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := handle.Stdout().Read(buf); err != nil {
				return
			}
		}
	}()

	select {
	case result := <-handle.Wait():
		if result.Err == nil {
			t.Fatal("expected wait error on websocket disconnect")
		}
		if !strings.Contains(result.Err.Error(), "sidecar lost upstream connection") {
			t.Fatalf("expected disconnect error to include sidecar detail, got %v", result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnect result")
	}
}

func TestWSHandle_RecoveredUnexpectedCloseCompletes(t *testing.T) {
	connected := make(chan struct{}, 1)
	closeConn := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(wsServerFrame{Type: "connected"}); err != nil {
			t.Fatalf("write connected: %v", err)
		}
		connected <- struct{}{}
		<-closeConn
	}))
	defer server.Close()

	handle := dialWSHandle(t, server, "container-recovered", -1)
	handle.setRecoveryInfo(&RecoveryInfo{
		SessionID: "sess-recovered",
		TaskID:    "task-recovered",
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket connect")
	}

	close(closeConn)

	select {
	case result := <-handle.Wait():
		if result.Err != nil {
			t.Fatalf("wait returned error: %v", result.Err)
		}
		if result.Code != 0 {
			t.Fatalf("expected zero exit code on recovered disconnect, got %d", result.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recovered disconnect result")
	}
}

func TestWSHandle_Kill(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WSHANDLE_DOCKER_LOG", logFile)
	installFakeDocker(t, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$WSHANDLE_DOCKER_LOG"
case "$1" in
  stop|rm)
    exit 0
    ;;
esac
echo "unexpected docker command: $1" >&2
exit 2
`)

	handle := &wsHandle{containerID: "container-kill"}
	if err := handle.Kill(); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read docker log: %v", err)
	}

	lines := strings.Fields(string(data))
	if len(lines) < 4 {
		t.Fatalf("expected docker stop and rm commands, got %q", string(data))
	}
	if lines[0] != "stop" || lines[1] != "container-kill" {
		t.Fatalf("expected docker stop command first, got %q", string(data))
	}
	if lines[2] != "rm" || lines[3] != "container-kill" {
		t.Fatalf("expected docker rm command second, got %q", string(data))
	}
}

func dialWSHandle(t *testing.T, server *httptest.Server, containerID string, sinceOffset int64) *wsHandle {
	t.Helper()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	hostPort := u.Port()
	handle, err := dialSidecar(containerID, hostPort, sinceOffset, "")
	if err != nil {
		t.Fatalf("dial sidecar: %v", err)
	}
	t.Cleanup(func() {
		if handle.cancel != nil {
			handle.cancel()
		}
		if handle.conn != nil {
			_ = handle.conn.Close()
		}
	})
	return handle
}

func wsClientFrameStringData(t *testing.T, frame wsClientFrame) string {
	t.Helper()
	data, err := json.Marshal(frame.Data)
	if err != nil {
		t.Fatalf("marshal frame data: %v", err)
	}
	var payload string
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal frame data as string: %v", err)
	}
	return payload
}
