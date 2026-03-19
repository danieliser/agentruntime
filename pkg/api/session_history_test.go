package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionHistory(t *testing.T) {
	ts, srv := newTestServer(t)

	// Create some test log files with result events
	logDir := srv.logDir

	// Create a completed session log
	sessionID1 := "test-session-1"
	logPath1 := filepath.Join(logDir, sessionID1+".ndjson")
	createMockLogFile(t, logPath1, map[string]interface{}{
		"agent":  "claude",
		"status": "completed",
		"exit_code": 0,
		"usage": map[string]interface{}{
			"input_tokens":  1000,
			"output_tokens": 500,
			"cost_usd":      0.0045,
		},
		"tool_calls": 3,
	})

	// Create a failed session log
	sessionID2 := "test-session-2"
	logPath2 := filepath.Join(logDir, sessionID2+".ndjson")
	createMockLogFile(t, logPath2, map[string]interface{}{
		"agent":  "codex",
		"status": "failed",
		"exit_code": 1,
		"usage": map[string]interface{}{
			"input_tokens":  500,
			"output_tokens": 200,
			"cost_usd":      0.0025,
		},
		"tool_calls": 1,
	})

	// Test GET /sessions/history
	resp := get(t, ts, "/sessions/history")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", resp.StatusCode)
	}

	var history []SessionHistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(history) != 2 {
		t.Errorf("Expected 2 history entries, got %d", len(history))
	}

	// Verify the entries
	entry1 := findHistoryEntry(history, sessionID1)
	if entry1 == nil {
		t.Fatalf("Session %s not found in history", sessionID1)
	}

	if entry1.Status != "completed" {
		t.Errorf("Expected status 'completed', got %s", entry1.Status)
	}
	if entry1.InputTokens != 1000 {
		t.Errorf("Expected 1000 input tokens, got %d", entry1.InputTokens)
	}
	if entry1.OutputTokens != 500 {
		t.Errorf("Expected 500 output tokens, got %d", entry1.OutputTokens)
	}
	if entry1.ToolCalls != 3 {
		t.Errorf("Expected 3 tool calls, got %d", entry1.ToolCalls)
	}
	if entry1.CostUSD != 0.0045 {
		t.Errorf("Expected cost 0.0045, got %f", entry1.CostUSD)
	}

	entry2 := findHistoryEntry(history, sessionID2)
	if entry2 == nil {
		t.Fatalf("Session %s not found in history", sessionID2)
	}

	if entry2.Status != "failed" {
		t.Errorf("Expected status 'failed', got %s", entry2.Status)
	}
	if entry2.InputTokens != 500 {
		t.Errorf("Expected 500 input tokens, got %d", entry2.InputTokens)
	}
}

func TestSessionHistoryWithLimit(t *testing.T) {
	ts, srv := newTestServer(t)

	logDir := srv.logDir

	// Create 5 session logs
	for i := 1; i <= 5; i++ {
		sessionID := fmt.Sprintf("test-session-%d", i)
		logPath := filepath.Join(logDir, sessionID+".ndjson")
		createMockLogFile(t, logPath, map[string]interface{}{
			"agent":  "claude",
			"status": "completed",
		})
	}

	// Test with limit=2
	resp := get(t, ts, "/sessions/history?limit=2")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", resp.StatusCode)
	}

	var history []SessionHistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(history) != 2 {
		t.Errorf("Expected 2 entries with limit=2, got %d", len(history))
	}
}

func TestSessionHistoryEmpty(t *testing.T) {
	ts, _ := newTestServer(t)

	// No log files created
	resp := get(t, ts, "/sessions/history")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", resp.StatusCode)
	}

	var history []SessionHistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(history) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(history))
	}
}

func TestSessionHistoryIgnoresNonNdjsonFiles(t *testing.T) {
	ts, srv := newTestServer(t)

	logDir := srv.logDir

	// Create an NDJSON file
	sessionID := "test-session-1"
	logPath := filepath.Join(logDir, sessionID+".ndjson")
	createMockLogFile(t, logPath, map[string]interface{}{
		"agent":  "claude",
		"status": "completed",
	})

	// Create a non-NDJSON file (should be ignored)
	otherPath := filepath.Join(logDir, "not-a-session.txt")
	if err := os.WriteFile(otherPath, []byte("random content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	resp := get(t, ts, "/sessions/history")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", resp.StatusCode)
	}

	var history []SessionHistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Should only have the NDJSON file, not the .txt file
	if len(history) != 1 {
		t.Errorf("Expected 1 entry (only NDJSON), got %d", len(history))
	}
}

// --- helper functions ---

func createMockLogFile(t *testing.T, path string, resultData map[string]interface{}) {
	t.Helper()

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create a mock NDJSON file with multiple events
	// The result event should be parsed by parseSessionLogTail
	events := []map[string]interface{}{
		{
			"type":      "agent_message",
			"data":      map[string]interface{}{"text": "Starting..."},
			"timestamp": time.Now().Add(-1 * time.Hour).UnixMilli(),
		},
		{
			"type":      "progress",
			"data":      map[string]interface{}{"message": "Working..."},
			"timestamp": time.Now().Add(-30 * time.Minute).UnixMilli(),
		},
		{
			"type":      "result",
			"data":      resultData,
			"timestamp": time.Now().UnixMilli(),
		},
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	defer f.Close()

	for _, event := range events {
		data, _ := json.Marshal(event)
		f.Write(data)
		f.WriteString("\n")
	}
}

func findHistoryEntry(history []SessionHistoryEntry, sessionID string) *SessionHistoryEntry {
	for i := range history {
		if history[i].SessionID == sessionID {
			return &history[i]
		}
	}
	return nil
}
