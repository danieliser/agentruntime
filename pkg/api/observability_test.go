package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestSessionGETReportsProviderSilenceAndActiveChildren(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	defer store.Close()
	now := time.Now().UTC().Add(-40 * time.Second)
	_, err := store.CreateSession(ctx, durable.CreateSessionParams{SessionID: "silence", IdempotencyKey: "silence", RequestHash: "hash", RequestManifest: json.RawMessage(`{}`), Agent: "codex", Runtime: "local", CreatedAt: now.Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "silence", Runtime: "local", ContainerID: "fixture", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	ts := newV1SessionTestServer(t, store, &atomic.Int32{})
	appendEvent := func(id, kind string, stream durable.Stream, payload string, at time.Time) {
		_, err := store.AppendEvent(ctx, durable.AppendEventParams{SchemaVersion: "1.0", EventID: id, SessionID: "silence", Generation: 1, Timestamp: at, Type: kind, Stream: stream, Payload: json.RawMessage(payload), Raw: []byte(payload)})
		if err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("start", "lifecycle.turn.started", durable.StreamProviderStdout, `{}`, now.Add(-time.Second))
	appendEvent("child", "subagent.started", durable.StreamProviderStdout, `{"subagent_thread_id":"child","subagent_path":"/root/check"}`, now)
	appendEvent("control", "control.steer.requested", durable.StreamControl, `{}`, time.Now())
	check := func(count float64) {
		resp, err := http.Get(ts.URL + "/api/v1/sessions/silence")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		p := body.Data
		if p["last_event_at"] != now.Format(time.RFC3339Nano) || p["active_subagents"] != count {
			t.Fatalf("activity=%v", p)
		}
		idle, ok := p["idle_seconds"].(float64)
		if !ok || idle < 40 {
			t.Fatalf("idle=%v", p)
		}
	}
	check(1)
	appendEvent("child-done", "subagent.completed", durable.StreamProviderStdout, `{"subagent_thread_id":"child","subagent_path":"/root/check"}`, now)
	appendEvent("done", "turn.completed", durable.StreamProviderStdout, `{}`, now)
	check(0)
}

func TestForwardedChildResultDoesNotWakeParentWatchers(t *testing.T) {
	sess := session.NewSession("", "claude", "local")
	done := sess.ResultCh()
	parseAndTrackEvent(sess, []byte(`{"type":"result","subtype":"success","parent_tool_use_id":"child-task"}`))
	select {
	case <-done:
		t.Fatal("child result completed parent watcher")
	default:
	}
	parseAndTrackEvent(sess, []byte(`{"type":"result","subtype":"success"}`))
	select {
	case <-done:
	default:
		t.Fatal("parent result did not complete watcher")
	}
}
