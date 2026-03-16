package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/api"
)

func TestClient_Dispatch(t *testing.T) {
	t.Parallel()

	var got api.SessionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/sessions" {
			t.Fatalf("expected /sessions, got %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(api.SessionResponse{
			SessionID: "sess-123",
			Agent:     "claude",
			Runtime:   "local",
			Status:    "running",
			WSURL:     "ws://example.test/ws/sessions/sess-123",
			LogURL:    "http://example.test/sessions/sess-123/logs",
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL)
	resp, err := client.Dispatch(context.Background(), api.SessionRequest{
		Agent:  "claude",
		Prompt: "do the thing",
		Tags:   map[string]string{"project": "agentruntime"},
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	if got.Agent != "claude" {
		t.Fatalf("expected agent claude, got %q", got.Agent)
	}
	if got.Prompt != "do the thing" {
		t.Fatalf("expected prompt to round-trip, got %q", got.Prompt)
	}
	if resp.SessionID != "sess-123" {
		t.Fatalf("expected session_id sess-123, got %q", resp.SessionID)
	}
}

func TestClient_GetSession(t *testing.T) {
	t.Parallel()

	createdAt := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/sessions/sess-456" {
			t.Fatalf("expected /sessions/sess-456, got %s", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(api.SessionSummary{
			SessionID: "sess-456",
			Agent:     "codex",
			Runtime:   "docker",
			Status:    "running",
			CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL)
	sess, err := client.GetSession(context.Background(), "sess-456")
	if err != nil {
		t.Fatalf("GetSession returned error: %v", err)
	}

	if sess.SessionID != "sess-456" {
		t.Fatalf("expected session_id sess-456, got %q", sess.SessionID)
	}
	if sess.Agent != "codex" {
		t.Fatalf("expected agent codex, got %q", sess.Agent)
	}
	if !sess.CreatedAt.Equal(createdAt) {
		t.Fatalf("expected created_at %v, got %v", createdAt, sess.CreatedAt)
	}
}

func TestClient_ListSessions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/sessions" {
			t.Fatalf("expected /sessions, got %s", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode([]api.SessionSummary{
			{SessionID: "a", Agent: "claude", Status: "running"},
			{SessionID: "b", Agent: "codex", Status: "completed"},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL)
	sessions, err := client.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}

	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestClient_Kill(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/sessions/sess-789" {
			t.Fatalf("expected /sessions/sess-789, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL)
	if err := client.Kill(context.Background(), "sess-789"); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}
}

func TestClient_GetLogs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/sessions/sess-log/logs" {
			t.Fatalf("expected /sessions/sess-log/logs, got %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("cursor"); got != "7" {
			t.Fatalf("expected cursor=7, got %q", got)
		}
		w.Header().Set("Agentruntime-Log-Cursor", "42")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("hello from agent\n")); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL)
	data, nextCursor, err := client.GetLogs(context.Background(), "sess-log", 7)
	if err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}

	if string(data) != "hello from agent\n" {
		t.Fatalf("expected log data, got %q", string(data))
	}
	if nextCursor != 42 {
		t.Fatalf("expected next cursor 42, got %d", nextCursor)
	}
}

func TestClient_GetLogs_NotFound(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"session not found"}`))
	}))
	defer server.Close()

	client := New(server.URL)
	if _, _, err := client.GetLogs(context.Background(), "missing", 0); err == nil {
		t.Fatal("expected error for missing session")
	}
}
