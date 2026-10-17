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
)

func TestStalledExitCode(t *testing.T) {
	for _, err := range []error{WaitReasonError("stalled"), fmt.Errorf("wrapped: %w", WaitReasonError("stalled"))} {
		if got := ExitCode(err); got != 17 {
			t.Fatalf("stalled code=%d, want distinct code 17", got)
		}
	}
}

func TestWaitStalledOnlyForSilentRunningTurns(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		running     bool
		idle        float64
		fresh       bool
		want        int
	}{
		{"silent running", "running", true, 40, false, 17},
		{"busy running", "running", true, 4, false, 0},
		{"warm idle", "running", false, 400, false, 0},
		{"completed", "completed", true, 400, false, 0},
		{"new prompt", "running", true, 400, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now().Add(-time.Minute)
			if tc.fresh {
				started = time.Now()
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"session_id": "s", "state": tc.state, "running_turn": tc.running, "idle_seconds": tc.idle, "turn_started_at": started, "active_subagents": 1}})
			}))
			defer srv.Close()
			cfg := Config{URL: srv.URL}
			out := new(strings.Builder)
			err := checkStalled(context.Background(), NewAPIClient(cfg), "s", 30*time.Second, cfg, out)
			if got := ExitCode(err); got != tc.want {
				t.Fatalf("code=%d err=%v output=%s", got, err, out)
			}
		})
	}
}

func TestSinceLabelsSubagentsAndHidesDeltas(t *testing.T) {
	got := summarizeEvents([]streamEvent{
		{Type: "subagent.content.completed", Payload: json.RawMessage(`{"subagent_path":"/root/check","text":"child done"}`)},
		{Type: "subagent.content.delta", Payload: json.RawMessage(`{"subagent_path":"/root/check","text":"noisy delta"}`)},
	}, false)
	if !strings.Contains(got, "[sub:/root/check] child done") || strings.Contains(got, "noisy delta") {
		t.Fatalf("output=%q", got)
	}
	if matchingReason(streamEvent{Type: "subagent.turn.completed"}) != "" {
		t.Fatal("child completed parent wait")
	}
}

func TestStatusIsReadableBeforeNativeTransportAttaches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/approvals"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_state","message":"native transport unavailable"}}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = w.Write([]byte(`{"data":{"events":[],"has_more":false}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"session_id":"s","state":"running","idle_seconds":1,"active_subagents":0}}`))
		}
	}))
	defer srv.Close()
	cfg := Config{URL: srv.URL}
	ctx := &Context{Config: cfg, API: NewAPIClient(cfg), Out: new(strings.Builder), Log: new(strings.Builder)}
	if err := cmdStatus(ctx, []string{"s"}); err != nil {
		t.Fatal(err)
	}
}
