package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeNDJSON writes NDJSON event lines to a file, returning the path.
func writeNDJSON(t *testing.T, dir, sessionID string, events []map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".ndjson")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create ndjson: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, evt := range events {
		if err := enc.Encode(evt); err != nil {
			t.Fatalf("encode event: %v", err)
		}
	}
	return path
}

func makeEvent(typ string, tsMillis int64, data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{"text": typ}
	}
	return map[string]any{
		"type":      typ,
		"timestamp": float64(tsMillis),
		"data":      data,
	}
}

func TestReadMessages_SingleSession(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UnixMilli()
	writeNDJSON(t, dir, "sess-1", []map[string]any{
		makeEvent("agent_message", ts, nil),
		makeEvent("tool_use", ts+1, nil),
		makeEvent("result", ts+2, nil),
	})

	r := NewLogReader(dir)
	entries, hasMore, err := r.ReadMessages([]string{"sess-1"}, 10, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasMore {
		t.Error("expected hasMore=false")
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Type != "agent_message" {
		t.Errorf("first type = %q, want agent_message", entries[0].Type)
	}
	if entries[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", entries[0].SessionID)
	}
	// Global offset for chainIndex=0, lineOffset=0 → 0
	if entries[0].Offset != 0 {
		t.Errorf("offset[0] = %d, want 0", entries[0].Offset)
	}
	if entries[1].Offset != 1 {
		t.Errorf("offset[1] = %d, want 1", entries[1].Offset)
	}
}

func TestReadMessages_MultiSession(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UnixMilli()
	writeNDJSON(t, dir, "sess-A", []map[string]any{
		makeEvent("agent_message", ts, nil),
	})
	writeNDJSON(t, dir, "sess-B", []map[string]any{
		makeEvent("result", ts+1000, nil),
		makeEvent("tool_use", ts+2000, nil),
	})

	r := NewLogReader(dir)
	entries, hasMore, err := r.ReadMessages([]string{"sess-A", "sess-B"}, 10, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasMore {
		t.Error("expected hasMore=false")
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// sess-A is chainIndex=0: offsets 0, 1, ...
	if entries[0].SessionID != "sess-A" {
		t.Errorf("entries[0].SessionID = %q, want sess-A", entries[0].SessionID)
	}
	if entries[0].Offset != 0 {
		t.Errorf("entries[0].Offset = %d, want 0", entries[0].Offset)
	}

	// sess-B is chainIndex=1: offsets 1*1e9 + 0, 1*1e9 + 1
	if entries[1].SessionID != "sess-B" {
		t.Errorf("entries[1].SessionID = %q, want sess-B", entries[1].SessionID)
	}
	if entries[1].Offset != 1_000_000_000 {
		t.Errorf("entries[1].Offset = %d, want 1000000000", entries[1].Offset)
	}
	if entries[2].Offset != 1_000_000_001 {
		t.Errorf("entries[2].Offset = %d, want 1000000001", entries[2].Offset)
	}
}

func TestReadMessages_Limit(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UnixMilli()
	writeNDJSON(t, dir, "sess-1", []map[string]any{
		makeEvent("agent_message", ts, nil),
		makeEvent("agent_message", ts+1, nil),
		makeEvent("agent_message", ts+2, nil),
		makeEvent("agent_message", ts+3, nil),
		makeEvent("agent_message", ts+4, nil),
	})

	r := NewLogReader(dir)
	entries, _, err := r.ReadMessages([]string{"sess-1"}, 3, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
}

func TestReadMessages_HasMore(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UnixMilli()
	writeNDJSON(t, dir, "sess-1", []map[string]any{
		makeEvent("agent_message", ts, nil),
		makeEvent("agent_message", ts+1, nil),
		makeEvent("agent_message", ts+2, nil),
	})

	r := NewLogReader(dir)
	entries, hasMore, err := r.ReadMessages([]string{"sess-1"}, 2, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasMore {
		t.Error("expected hasMore=true")
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestReadMessages_BeforeCursor(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UnixMilli()
	writeNDJSON(t, dir, "sess-1", []map[string]any{
		makeEvent("agent_message", ts, nil),   // offset 0
		makeEvent("agent_message", ts+1, nil), // offset 1
		makeEvent("agent_message", ts+2, nil), // offset 2
	})

	r := NewLogReader(dir)
	// before=2: only offsets 0 and 1 qualify
	entries, hasMore, err := r.ReadMessages([]string{"sess-1"}, 10, 2, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasMore {
		t.Error("expected hasMore=false")
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[1].Offset != 1 {
		t.Errorf("last entry offset = %d, want 1", entries[1].Offset)
	}
}

func TestReadMessages_TypeFilter(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UnixMilli()
	writeNDJSON(t, dir, "sess-1", []map[string]any{
		makeEvent("agent_message", ts, nil),
		makeEvent("tool_use", ts+1, nil),
		makeEvent("result", ts+2, nil),
		makeEvent("progress", ts+3, nil),
	})

	r := NewLogReader(dir)
	entries, _, err := r.ReadMessages([]string{"sess-1"}, 10, 0, []string{"agent_message", "result"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Type != "agent_message" {
		t.Errorf("entries[0].Type = %q, want agent_message", entries[0].Type)
	}
	if entries[1].Type != "result" {
		t.Errorf("entries[1].Type = %q, want result", entries[1].Type)
	}
}

func TestReadMessages_MissingLogFile(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UnixMilli()
	// Only write sess-B; sess-A is missing.
	writeNDJSON(t, dir, "sess-B", []map[string]any{
		makeEvent("agent_message", ts, nil),
	})

	r := NewLogReader(dir)
	entries, hasMore, err := r.ReadMessages([]string{"sess-A", "sess-B"}, 10, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasMore {
		t.Error("expected hasMore=false")
	}
	// sess-A skipped; sess-B (chainIndex=1) has one entry at offset 1*1e9+0
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].SessionID != "sess-B" {
		t.Errorf("expected sess-B, got %q", entries[0].SessionID)
	}
	if entries[0].Offset != 1_000_000_000 {
		t.Errorf("offset = %d, want 1000000000", entries[0].Offset)
	}
}

func TestReadMessages_EmptyChain(t *testing.T) {
	dir := t.TempDir()
	r := NewLogReader(dir)
	entries, hasMore, err := r.ReadMessages(nil, 10, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasMore {
		t.Error("expected hasMore=false")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}

	// Also test empty slice (not nil).
	entries, hasMore, err = r.ReadMessages([]string{}, 10, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasMore || len(entries) != 0 {
		t.Error("expected empty result for empty chain")
	}
}
