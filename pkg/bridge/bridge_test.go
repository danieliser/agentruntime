package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/session"
)

func TestServerFrame_JSONRoundtrip(t *testing.T) {
	code := 0
	frame := ServerFrame{
		Type:     "exit",
		ExitCode: &code,
	}

	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded ServerFrame
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.Type != "exit" {
		t.Fatalf("expected type 'exit', got %q", decoded.Type)
	}
	if decoded.ExitCode == nil || *decoded.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %v", decoded.ExitCode)
	}
}

func TestClientFrame_JSONRoundtrip(t *testing.T) {
	frame := ClientFrame{
		Type: "stdin",
		Data: "hello\n",
	}

	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded ClientFrame
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.Type != "stdin" {
		t.Fatalf("expected type 'stdin', got %q", decoded.Type)
	}
	if decoded.Data != "hello\n" {
		t.Fatalf("expected data 'hello\\n', got %q", decoded.Data)
	}
}

func TestServerFrame_StdoutType(t *testing.T) {
	frame := ServerFrame{
		Type: "stdout",
		Data: "output line\n",
	}
	data, _ := json.Marshal(frame)
	if !strings.Contains(string(data), `"type":"stdout"`) {
		t.Fatalf("expected stdout type in JSON, got %s", data)
	}
}

func TestServerFrame_ReplayType(t *testing.T) {
	frame := ServerFrame{
		Type:   "replay",
		Data:   "cmVwbGF5ZWQ=", // base64
		Offset: 42,
	}
	data, _ := json.Marshal(frame)
	if !strings.Contains(string(data), `"replay"`) {
		t.Fatalf("expected replay type in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `"offset":42`) {
		t.Fatalf("expected offset in JSON, got %s", data)
	}
}

func TestReplayOnReconnect(t *testing.T) {
	replay := session.NewReplayBuffer(1024)
	replay.Write([]byte("line 1\n"))
	replay.Write([]byte("line 2\n"))

	// Simulate reconnect from offset 0.
	data, nextOffset := replay.ReadFrom(0)
	if string(data) != "line 1\nline 2\n" {
		t.Fatalf("expected full replay, got %q", string(data))
	}
	if nextOffset != 14 {
		t.Fatalf("expected offset 14, got %d", nextOffset)
	}

	// Simulate reconnect from offset 7 (after first line).
	data, nextOffset = replay.ReadFrom(7)
	if string(data) != "line 2\n" {
		t.Fatalf("expected partial replay, got %q", string(data))
	}
	if nextOffset != 14 {
		t.Fatalf("expected offset 14, got %d", nextOffset)
	}
}

// TestBridge_WSConnectedFrame verifies WebSocket frame flow using a real
// WS connection against a test server that simulates bridge behavior.
func TestBridge_WSConnectedFrame(t *testing.T) {
	replay := session.NewReplayBuffer(1024)
	replay.Write([]byte("prior output\n"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Send replay.
		data, offset := replay.ReadFrom(0)
		_ = conn.WriteJSON(ServerFrame{
			Type:   "replay",
			Data:   string(data),
			Offset: offset,
		})

		// Send connected.
		_ = conn.WriteJSON(ServerFrame{
			Type:      "connected",
			SessionID: "test-session",
			Mode:      "pipe",
		})
	}))
	defer server.Close()

	// Connect WebSocket client.
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read replay frame.
	var replayFrame ServerFrame
	if err := conn.ReadJSON(&replayFrame); err != nil {
		t.Fatalf("read replay frame failed: %v", err)
	}
	if replayFrame.Type != "replay" {
		t.Fatalf("expected replay frame, got %q", replayFrame.Type)
	}
	if replayFrame.Data != "prior output\n" {
		t.Fatalf("expected replay data, got %q", replayFrame.Data)
	}

	// Read connected frame.
	var connFrame ServerFrame
	if err := conn.ReadJSON(&connFrame); err != nil {
		t.Fatalf("read connected frame failed: %v", err)
	}
	if connFrame.Type != "connected" {
		t.Fatalf("expected connected frame, got %q", connFrame.Type)
	}
	if connFrame.SessionID != "test-session" {
		t.Fatalf("expected session id 'test-session', got %q", connFrame.SessionID)
	}
}

// TestBridge_WSPingPong verifies application-level ping/pong frames.
func TestBridge_WSPingPong(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Read a client frame and respond.
		var frame ClientFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		if frame.Type == "ping" {
			_ = conn.WriteJSON(ServerFrame{Type: "pong"})
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send ping.
	_ = conn.WriteJSON(ClientFrame{Type: "ping"})

	// Read pong.
	var frame ServerFrame
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("read pong failed: %v", err)
	}
	if frame.Type != "pong" {
		t.Fatalf("expected pong, got %q", frame.Type)
	}
}
