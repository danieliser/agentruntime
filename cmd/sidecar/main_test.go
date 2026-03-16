package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	bridgepkg "github.com/danieliser/agentruntime/pkg/bridge"
)

func TestSidecar_HealthEndpoint(t *testing.T) {
	sc, ts := newTestSidecar(t, []string{"echo", "hello"})
	defer sc.stop()

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
		t.Fatal("expected agent_running false before first websocket connect")
	}
}

func TestSidecar_WSEchoOutput(t *testing.T) {
	sc, ts := newTestSidecar(t, []string{"echo", "hello"})
	defer sc.stop()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	var gotHello bool
	var gotExit bool
	for i := 0; i < 8; i++ {
		frame := readFrame(t, conn)
		if frame.Type == "stdout" && strings.Contains(frame.Data, "hello") {
			gotHello = true
		}
		if frame.Type == "exit" {
			gotExit = true
			break
		}
	}

	if !gotHello {
		t.Fatal("expected stdout frame containing hello")
	}
	if !gotExit {
		t.Fatal("expected exit frame")
	}
}

func TestSidecar_WSReplay(t *testing.T) {
	sc, ts := newTestSidecar(t, []string{"echo", "hello"})
	defer sc.stop()

	conn1 := mustDialWS(t, ts, "")
	for i := 0; i < 8; i++ {
		frame := readFrame(t, conn1)
		if frame.Type == "exit" {
			break
		}
	}
	conn1.Close()

	conn2 := mustDialWS(t, ts, "?since=0")
	defer conn2.Close()

	var gotReplay bool
	for i := 0; i < 8; i++ {
		frame := readFrame(t, conn2)
		if frame.Type == "replay" {
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				t.Fatalf("decode replay: %v", err)
			}
			if strings.Contains(string(data), "hello") {
				gotReplay = true
				break
			}
		}
	}

	if !gotReplay {
		t.Fatal("expected replay frame containing hello")
	}
}

func TestSidecar_WSStdinRouting(t *testing.T) {
	sc, ts := newTestSidecar(t, []string{"cat"})
	defer sc.stop()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	waitForConnected(t, conn)

	if err := conn.WriteJSON(bridgepkg.ClientFrame{
		Type: "stdin",
		Data: "hello from stdin\n",
	}); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}

	for i := 0; i < 8; i++ {
		frame := readFrame(t, conn)
		if frame.Type == "stdout" && strings.Contains(frame.Data, "hello from stdin") {
			return
		}
	}

	t.Fatal("expected echoed stdout frame from cat")
}

func TestSidecar_WSPingPong(t *testing.T) {
	sc, ts := newTestSidecar(t, []string{"cat"})
	defer sc.stop()

	conn := mustDialWS(t, ts, "")
	defer conn.Close()

	waitForConnected(t, conn)

	if err := conn.WriteJSON(bridgepkg.ClientFrame{Type: "ping"}); err != nil {
		t.Fatalf("write ping frame: %v", err)
	}

	for i := 0; i < 8; i++ {
		frame := readFrame(t, conn)
		if frame.Type == "pong" {
			return
		}
	}

	t.Fatal("expected pong frame")
}

func newTestSidecar(t *testing.T, cmd []string) (*sidecar, *httptest.Server) {
	t.Helper()

	raw, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal AGENT_CMD: %v", err)
	}
	t.Setenv("AGENT_CMD", string(raw))

	sc, _, err := newSidecarFromEnv()
	if err != nil {
		t.Fatalf("new sidecar: %v", err)
	}

	ts := httptest.NewServer(sc.routes())
	t.Cleanup(ts.Close)
	t.Cleanup(sc.stop)
	return sc, ts
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

func readFrame(t *testing.T, conn *websocket.Conn) bridgepkg.ServerFrame {
	t.Helper()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var frame bridgepkg.ServerFrame
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame
}

func waitForConnected(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	for i := 0; i < 8; i++ {
		frame := readFrame(t, conn)
		if frame.Type == "connected" {
			return
		}
	}

	t.Fatal("expected connected frame")
}
