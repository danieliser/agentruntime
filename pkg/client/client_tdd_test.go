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

// --- TDD tests for the Go client. Written RED before implementation. ---

func TestClient_Dispatch_SendsCorrectBody(t *testing.T) {
	var receivedBody api.SessionRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/sessions" {
			t.Fatalf("expected /sessions, got %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.SessionResponse{
			SessionID: "sess-123",
			Agent:     "claude",
			Runtime:   "local",
			Status:    "running",
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	resp, err := c.Dispatch(context.Background(), api.SessionRequest{
		Agent:  "claude",
		Prompt: "do the thing",
		Tags:   map[string]string{"project": "agentruntime"},
	})
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if resp.SessionID != "sess-123" {
		t.Fatalf("expected session_id 'sess-123', got %q", resp.SessionID)
	}
	if receivedBody.Agent != "claude" {
		t.Fatalf("expected agent 'claude' in body, got %q", receivedBody.Agent)
	}
	if receivedBody.Prompt != "do the thing" {
		t.Fatalf("expected prompt in body, got %q", receivedBody.Prompt)
	}
}

func TestClient_GetSession_ReturnsSession(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/sessions/sess-456" {
			t.Fatalf("expected /sessions/sess-456, got %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(api.SessionSummary{
			SessionID: "sess-456",
			Agent:     "codex",
			Runtime:   "docker",
			Status:    "running",
			CreatedAt: time.Now(),
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	sess, err := c.GetSession(context.Background(), "sess-456")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if sess.SessionID != "sess-456" {
		t.Fatalf("expected session_id 'sess-456', got %q", sess.SessionID)
	}
	if sess.Agent != "codex" {
		t.Fatalf("expected agent 'codex', got %q", sess.Agent)
	}
}

func TestClient_ListSessions_ReturnsArray(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions" || r.Method != http.MethodGet {
			t.Fatalf("expected GET /sessions, got %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]api.SessionSummary{
			{SessionID: "a", Agent: "claude", Status: "running"},
			{SessionID: "b", Agent: "codex", Status: "completed"},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestClient_Kill_SendsDelete(t *testing.T) {
	var method, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "killed"})
	}))
	defer ts.Close()

	c := New(ts.URL)
	err := c.Kill(context.Background(), "sess-789")
	if err != nil {
		t.Fatalf("Kill failed: %v", err)
	}
	if method != http.MethodDelete {
		t.Fatalf("expected DELETE, got %s", method)
	}
	if path != "/sessions/sess-789" {
		t.Fatalf("expected /sessions/sess-789, got %s", path)
	}
}

func TestClient_GetLogs_ParsesCursorHeader(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/sess-log/logs" {
			t.Fatalf("expected /sessions/sess-log/logs, got %s", r.URL.Path)
		}
		cursor := r.URL.Query().Get("cursor")
		if cursor != "0" {
			t.Fatalf("expected cursor=0, got %q", cursor)
		}
		w.Header().Set("Agentruntime-Log-Cursor", "42")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from agent\n"))
	}))
	defer ts.Close()

	c := New(ts.URL)
	data, nextCursor, err := c.GetLogs(context.Background(), "sess-log", 0)
	if err != nil {
		t.Fatalf("GetLogs failed: %v", err)
	}
	if string(data) != "hello from agent\n" {
		t.Fatalf("expected log data, got %q", string(data))
	}
	if nextCursor != 42 {
		t.Fatalf("expected next cursor 42, got %d", nextCursor)
	}
}

func TestClient_GetLogs_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"session not found"}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	_, _, err := c.GetLogs(context.Background(), "nonexistent", 0)
	if err == nil {
		t.Fatal("expected error for 404 session")
	}
}

func TestClient_Dispatch_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"spawn failed"}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	_, err := c.Dispatch(context.Background(), api.SessionRequest{
		Agent:  "claude",
		Prompt: "fail",
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
