package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAttachConnectsToWS(t *testing.T) {
	// Create a mock WebSocket server that echoes back a connected frame
	upgrader := websocket.Upgrader{}
	connectedOnce := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/sessions/test-id" {
			http.NotFound(w, r)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		// Send connected frame
		if !connectedOnce {
			connectedOnce = true
			_ = conn.WriteJSON(ServerFrame{
				Type:      "connected",
				SessionID: "test-id",
				Mode:      "pipe",
			})

			// Send a single message
			data := `{"type":"agent_message","data":{"text":"hello"}}`
			encoded := base64.StdEncoding.EncodeToString([]byte(data))
			_ = conn.WriteJSON(ServerFrame{
				Type: "stdout",
				Data: encoded,
			})

			// Send exit
			code := 0
			_ = conn.WriteJSON(ServerFrame{
				Type:     "exit",
				ExitCode: &code,
			})
		}
	}))
	defer server.Close()

	// Extract port from server URL
	parts := strings.Split(server.URL, ":")
	if len(parts) < 3 {
		t.Fatalf("unexpected server URL: %s", server.URL)
	}
	port := parts[2]

	// Run attach with a timeout
	done := make(chan error, 1)
	go func() {
		done <- attach("test-id", parsePort(port), 0, false)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach timeout")
	}
}

func TestAttachSendsStdinFrame(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var clientFrames []ClientFrame
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/sessions/test-id" {
			http.NotFound(w, r)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		// Send connected
		_ = conn.WriteJSON(ServerFrame{
			Type:      "connected",
			SessionID: "test-id",
		})

		// Receive frames from client
		for {
			var frame ClientFrame
			err := conn.ReadJSON(&frame)
			if err != nil {
				break
			}

			mu.Lock()
			clientFrames = append(clientFrames, frame)
			mu.Unlock()

			// Send exit after receiving a stdin frame
			if frame.Type == "stdin" {
				code := 0
				_ = conn.WriteJSON(ServerFrame{
					Type:     "exit",
					ExitCode: &code,
				})
				break
			}
		}
	}))
	defer server.Close()

	parts := strings.Split(server.URL, ":")
	if len(parts) < 3 {
		t.Fatalf("unexpected server URL: %s", server.URL)
	}
	port := parts[2]

	done := make(chan error, 1)
	stdin, stdinWrite, _ := newTestStdin("test input\n")
	defer stdin.Close()
	defer stdinWrite.Close()

	go func() {
		// Redirect stdin
		oldStdin := os.Stdin
		os.Stdin = stdin
		defer func() { os.Stdin = oldStdin }()

		done <- attach("test-id", parsePort(port), 0, false)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach timeout")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(clientFrames) != 1 {
		t.Fatalf("expected 1 client frame, got %d", len(clientFrames))
	}

	if clientFrames[0].Type != "stdin" {
		t.Fatalf("expected stdin frame, got %s", clientFrames[0].Type)
	}

	if !strings.Contains(clientFrames[0].Data, "test input") {
		t.Fatalf("expected 'test input' in data, got %q", clientFrames[0].Data)
	}
}

func TestAttachSendsSteerFrame(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var clientFrames []ClientFrame
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/sessions/test-id" {
			http.NotFound(w, r)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		_ = conn.WriteJSON(ServerFrame{
			Type:      "connected",
			SessionID: "test-id",
		})

		for {
			var frame ClientFrame
			err := conn.ReadJSON(&frame)
			if err != nil {
				break
			}

			mu.Lock()
			clientFrames = append(clientFrames, frame)
			mu.Unlock()

			if frame.Type == "steer" {
				code := 0
				_ = conn.WriteJSON(ServerFrame{
					Type:     "exit",
					ExitCode: &code,
				})
				break
			}
		}
	}))
	defer server.Close()

	parts := strings.Split(server.URL, ":")
	if len(parts) < 3 {
		t.Fatalf("unexpected server URL: %s", server.URL)
	}
	port := parts[2]

	done := make(chan error, 1)
	stdin, stdinWrite, _ := newTestStdin("/steer fix the thing\n")
	defer stdin.Close()
	defer stdinWrite.Close()

	go func() {
		oldStdin := os.Stdin
		os.Stdin = stdin
		defer func() { os.Stdin = oldStdin }()

		done <- attach("test-id", parsePort(port), 0, false)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach timeout")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(clientFrames) != 1 {
		t.Fatalf("expected 1 client frame, got %d", len(clientFrames))
	}

	if clientFrames[0].Type != "steer" {
		t.Fatalf("expected steer frame, got %s", clientFrames[0].Type)
	}

	if !strings.Contains(clientFrames[0].Data, "fix the thing") {
		t.Fatalf("expected 'fix the thing' in data, got %q", clientFrames[0].Data)
	}
}

func TestAttachParsesNDJSON(t *testing.T) {
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/sessions/test-id" {
			http.NotFound(w, r)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		_ = conn.WriteJSON(ServerFrame{
			Type:      "connected",
			SessionID: "test-id",
		})

		// Send NDJSON event
		eventJSON := `{"type":"agent_message","data":{"text":"hello world"}}`
		encoded := base64.StdEncoding.EncodeToString([]byte(eventJSON))
		_ = conn.WriteJSON(ServerFrame{
			Type: "stdout",
			Data: encoded,
		})

		code := 0
		_ = conn.WriteJSON(ServerFrame{
			Type:     "exit",
			ExitCode: &code,
		})
	}))
	defer server.Close()

	parts := strings.Split(server.URL, ":")
	if len(parts) < 3 {
		t.Fatalf("unexpected server URL: %s", server.URL)
	}
	port := parts[2]

	done := make(chan error, 1)
	go func() {
		done <- attach("test-id", parsePort(port), 0, false)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach timeout")
	}
}

func TestHandleServerFrameParseNDJSON(t *testing.T) {
	tests := []struct {
		name    string
		frame   ServerFrame
		wantErr bool
	}{
		{
			name: "agent_message",
			frame: ServerFrame{
				Type: "stdout",
				Data: base64.StdEncoding.EncodeToString(
					[]byte(`{"type":"agent_message","data":{"text":"hello"}}`),
				),
			},
			wantErr: false,
		},
		{
			name: "tool_use",
			frame: ServerFrame{
				Type: "stdout",
				Data: base64.StdEncoding.EncodeToString(
					[]byte(`{"type":"tool_use","data":{"name":"Bash"}}`),
				),
			},
			wantErr: false,
		},
		{
			name: "exit",
			frame: ServerFrame{
				Type:     "exit",
				ExitCode: intPtr(0),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := handleServerFrame(&tt.frame)
			if (err != nil) != tt.wantErr {
				t.Fatalf("wantErr=%v, got err=%v", tt.wantErr, err)
			}
		})
	}
}

// Helper functions

func newTestStdin(data string) (*os.File, *os.File, error) {
	// This is a simplified mock. In a real test, use io.Pipe.
	// For now, we'll just return a way to read from a string buffer.
	// Actually, let's use proper pipes:
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	go func() {
		w.WriteString(data)
		w.Close()
	}()
	return r, w, nil
}

func parsePort(portStr string) int {
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return port
}

func intPtr(i int) *int {
	return &i
}
