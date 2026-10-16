package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSinceReturnsAssistantAndToolSummariesThenEmpty(t *testing.T) {
	// The direct event summarizer is also used by WebSocket wait.
	events := []streamEvent{
		{Sequence: 1, Type: "content.delta", Payload: json.RawMessage(`{"text":"hello"}`)},
		{Sequence: 2, Type: "tool.call", Payload: json.RawMessage(`{"name":"read_file"}`)},
		{Sequence: 3, Type: "turn.completed"},
	}
	got := summarizeEvents(events, false)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "read_file") {
		t.Fatalf("summaries = %q", got)
	}
}

func TestSincePersistsCursorAndSecondReadIsEmpty(t *testing.T) {
	var after []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after = append(after, r.URL.Query().Get("after_sequence"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_sequence") == "0" {
			_, _ = w.Write([]byte(`{"data":{"events":[{"schema_version":"1.0","event_id":"e1","session_id":"s1","generation":1,"sequence":1,"type":"content.delta","stream":"provider_stdout","payload":{"text":"hello caller"},"raw_base64":"eyJ0eXBlIjoidGV4dF9kZWx0YSJ9"},{"schema_version":"1.0","event_id":"e2","session_id":"s1","generation":1,"sequence":2,"type":"tool.call","stream":"provider_stdout","payload":{"name":"read_file"},"raw_base64":"eyJ0eXBlIjoidG9vbF91c2UifQ=="}],"earliest_sequence":1,"last_sequence":2,"has_more":false}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"events":[],"earliest_sequence":1,"last_sequence":2,"has_more":false}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	ctx := &Context{Config: Config{URL: srv.URL, Caller: "worker A"}, API: NewAPIClient(Config{URL: srv.URL}), Out: &strings.Builder{}, Log: &strings.Builder{}}
	if err := cmdSince(ctx, []string{"s1", "--cursor-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if got := ctx.Out.(*strings.Builder).String(); !strings.Contains(got, "hello caller") || !strings.Contains(got, "read_file") {
		t.Fatalf("output %q", got)
	}
	ctx.Out = &strings.Builder{}
	if err := cmdSince(ctx, []string{"s1", "--cursor-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if ctx.Out.(*strings.Builder).Len() != 0 {
		t.Fatalf("second output %q", ctx.Out.(*strings.Builder).String())
	}
	if fmt.Sprint(after) != "[0 2]" {
		t.Fatalf("after cursors %v", after)
	}
}

func TestWaitReconnectsFromLastContiguousSequenceAndMapsReason(t *testing.T) {
	var connections int
	var cursors []string
	upgrader := websocket.Upgrader{Subprotocols: []string{"agentd.v1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections++
		cursors = append(cursors, r.URL.Query().Get("after_sequence"))
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if connections == 1 {
			_ = conn.WriteJSON(map[string]any{"frame_type": "stream.ready", "after_sequence": 0})
			_ = conn.WriteJSON(map[string]any{"sequence": 1, "type": "content.delta", "payload": map[string]string{"text": "partial"}})
			return // force reconnect
		}
		_ = conn.WriteJSON(map[string]any{"frame_type": "stream.ready", "after_sequence": 1})
		_ = conn.WriteJSON(map[string]any{"sequence": 2, "type": "turn.completed", "payload": map[string]string{"text": "finished"}})
	}))
	defer srv.Close()
	cfg := Config{URL: "http" + strings.TrimPrefix(srv.URL, "http"), Caller: "test", Token: "token"}
	out := &strings.Builder{}
	err := waitStream(context.Background(), cfg, "s-1", CursorStore{Dir: t.TempDir()}, []string{"turn_end"}, time.Second, out)
	if ExitCode(err) != ExitWaitTurnEnd {
		t.Fatalf("reason %v code %d", err, ExitCode(err))
	}
	if connections < 2 || fmt.Sprint(cursors[:2]) != "[0 1]" {
		t.Fatalf("connections=%d cursors=%v", connections, cursors)
	}
	if !strings.Contains(out.String(), "turn.completed") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestWaitExitCodeTable(t *testing.T) {
	for _, tc := range []struct {
		reason string
		code   int
	}{{"turn_end", 10}, {"approval", 11}, {"error", 12}, {"done", 13}} {
		if got := ExitCode(WaitReasonError(tc.reason)); got != tc.code {
			t.Errorf("%s: %d", tc.reason, got)
		}
	}
}

func TestStreamEventFrameDecodesSequence(t *testing.T) {
	var got streamEvent
	if err := json.Unmarshal([]byte(`{"sequence":4,"type":"control.approval.request"}`), &got); err != nil || got.Sequence != 4 {
		t.Fatal(err, got)
	}
	_ = fmt.Sprint
}
