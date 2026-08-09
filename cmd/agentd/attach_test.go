package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAttachUsesDurableEventStreamSequenceCursor(t *testing.T) {
	upgrader := websocket.Upgrader{}
	requestSeen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen <- r.URL.RequestURI()
		if r.URL.Path != "/api/v1/ws/sessions/test-id/events" || r.URL.Query().Get("after_sequence") != "4" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		writeReady(t, conn, "test-id", 4, 5)
		writeTerminal(t, conn, 5, "completed", 0)
	}))
	defer server.Close()
	if err := attach("test-id", testServerPort(t, server), 4, false); err != nil {
		t.Fatalf("attach durable stream: %v", err)
	}
	select {
	case uri := <-requestSeen:
		if uri != "/api/v1/ws/sessions/test-id/events?after_sequence=4" {
			t.Fatalf("durable stream URI = %q", uri)
		}
	case <-time.After(time.Second):
		t.Fatal("durable event stream was not requested")
	}
}

func TestAttachNoReplayStartsAtDurableTail(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions/test-id":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"last_sequence": 8}})
		case "/api/v1/ws/sessions/test-id/events":
			if r.URL.Query().Get("after_sequence") != "8" {
				t.Errorf("after_sequence = %q, want 8", r.URL.Query().Get("after_sequence"))
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			writeReady(t, conn, "test-id", 8, 8)
			writeTerminal(t, conn, 9, "completed", 0)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := attach("test-id", testServerPort(t, server), 0, true); err != nil {
		t.Fatalf("attach without replay: %v", err)
	}
}

func TestAttachSendsFollowUpThroughVersionedInput(t *testing.T) {
	testAttachControl(t, "hello again\n", "input", "prompt", "hello again")
}

func TestAttachSendsSteerThroughVersionedInput(t *testing.T) {
	testAttachControl(t, "/steer fix the thing\n", "input", "steer", "fix the thing")
}

func TestHandleEventStreamFrameUsesDerivedPayloadAndTerminal(t *testing.T) {
	if err := handleEventStreamFrame(&EventStreamFrame{
		FrameType: "stream.ready", SessionID: "session", ReplayThrough: 3,
	}, 3); err != nil {
		t.Fatalf("ready frame: %v", err)
	}
	if err := handleEventStreamFrame(&EventStreamFrame{
		EventID: "evt-content", Sequence: 3, Type: "content.delta", Stream: "provider_stdout",
		Payload: json.RawMessage(`{"text":"hello"}`),
	}, 3); err != nil {
		t.Fatalf("content event: %v", err)
	}
	err := handleEventStreamFrame(&EventStreamFrame{
		EventID: "evt-terminal", Sequence: 4, Type: "session.crashed", Stream: "terminal",
		Payload: json.RawMessage(`{"reason":"crashed","exit_code":137,"signal":"SIGKILL"}`),
	}, 3)
	if err != errSessionExit {
		t.Fatalf("terminal event error = %v", err)
	}
}

func testAttachControl(t *testing.T, stdinValue, wantOperation, wantKind, wantText string) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ws/sessions/test-id/events":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			writeReady(t, conn, "test-id", 0, 0)
			select {
			case <-received:
				writeTerminal(t, conn, 1, "completed", 0)
			case <-time.After(2 * time.Second):
				t.Error("control was not received")
			}
		case "/api/v1/sessions/test-id/" + wantOperation:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode control: %v", err)
			}
			if body["idempotency_key"] == "" || body["kind"] != wantKind || body["text"] != wantText {
				t.Errorf("control body = %+v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			received <- body
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	stdin, stdinWrite, err := newTestStdin(stdinValue)
	if err != nil {
		t.Fatalf("test stdin: %v", err)
	}
	defer stdin.Close()
	defer stdinWrite.Close()
	if err := attach("test-id", testServerPort(t, server), 0, false, stdin); err != nil {
		t.Fatalf("attach control: %v", err)
	}
}

func writeReady(t *testing.T, conn *websocket.Conn, sessionID string, after, replayThrough int64) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{
		"frame_type": "stream.ready", "schema_version": "1.0", "session_id": sessionID,
		"after_sequence": after, "earliest_sequence": 1, "replay_through": replayThrough,
	}); err != nil {
		t.Errorf("write ready: %v", err)
	}
}

func writeTerminal(t *testing.T, conn *websocket.Conn, sequence int64, reason string, exitCode int) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"reason":%q,"exit_code":%d}`, reason, exitCode))
	if err := conn.WriteJSON(map[string]any{
		"schema_version": "1.0", "event_id": fmt.Sprintf("evt-%d", sequence), "session_id": "test-id",
		"generation": 1, "sequence": sequence, "type": "session." + reason, "stream": "terminal",
		"payload": json.RawMessage(raw), "raw_base64": base64.StdEncoding.EncodeToString(raw),
	}); err != nil {
		t.Errorf("write terminal: %v", err)
	}
}

func newTestStdin(data string) (*os.File, *os.File, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	go func() {
		_, _ = writer.WriteString(data)
		_ = writer.Close()
	}()
	return reader, writer, nil
}

func testServerPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	return parsePort(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
}

func parsePort(portString string) int {
	var port int
	_, _ = fmt.Sscanf(portString, "%d", &port)
	return port
}
