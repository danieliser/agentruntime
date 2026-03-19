package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLogRange_FullRange(t *testing.T) {
	// Create a temporary log file with known content.
	tmpDir := t.TempDir()
	sessionID := "test-session"
	logPath := LogFilePath(tmpDir, sessionID)

	content := []byte("hello world from log file")
	if err := os.WriteFile(logPath, content, 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	// Read the full range.
	data, err := ReadLogRange(tmpDir, sessionID, 0, int64(len(content)))
	if err != nil {
		t.Fatalf("ReadLogRange failed: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("got %q, want %q", string(data), string(content))
	}
}

func TestReadLogRange_PartialRange(t *testing.T) {
	// Create a temporary log file.
	tmpDir := t.TempDir()
	sessionID := "test-session"
	logPath := LogFilePath(tmpDir, sessionID)

	content := []byte("hello world from log file")
	if err := os.WriteFile(logPath, content, 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	// Read a partial range (offset 6, length 5: "world").
	data, err := ReadLogRange(tmpDir, sessionID, 6, 11)
	if err != nil {
		t.Fatalf("ReadLogRange failed: %v", err)
	}
	if string(data) != "world" {
		t.Errorf("got %q, want %q", string(data), "world")
	}
}

func TestReadLogRange_OffsetBeyondFile(t *testing.T) {
	// Create a small log file.
	tmpDir := t.TempDir()
	sessionID := "test-session"
	logPath := LogFilePath(tmpDir, sessionID)

	content := []byte("short")
	if err := os.WriteFile(logPath, content, 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	// Try to read beyond file size.
	data, err := ReadLogRange(tmpDir, sessionID, 100, 200)
	if err != nil {
		t.Fatalf("ReadLogRange failed: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty data beyond file, got %q", string(data))
	}
}

func TestReadLogRange_FileNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "nonexistent-session"

	// Try to read from a nonexistent file.
	data, err := ReadLogRange(tmpDir, sessionID, 0, 100)
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
	if data != nil {
		t.Errorf("expected nil data, got %v", data)
	}
}

func TestReadLogRange_ZeroLengthRange(t *testing.T) {
	// Create a temporary log file.
	tmpDir := t.TempDir()
	sessionID := "test-session"
	logPath := LogFilePath(tmpDir, sessionID)

	content := []byte("hello world")
	if err := os.WriteFile(logPath, content, 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	// Request zero-length range.
	data, err := ReadLogRange(tmpDir, sessionID, 5, 5)
	if err != nil {
		t.Fatalf("ReadLogRange failed: %v", err)
	}
	if data != nil {
		t.Errorf("expected nil for zero-length range, got %v", data)
	}
}

func TestReadLogRange_PartialRead(t *testing.T) {
	// Create a temporary log file.
	tmpDir := t.TempDir()
	sessionID := "test-session"
	logPath := LogFilePath(tmpDir, sessionID)

	content := []byte("hello world")
	if err := os.WriteFile(logPath, content, 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	// Request more bytes than available (from offset 6 to 100, file is only 11 bytes).
	data, err := ReadLogRange(tmpDir, sessionID, 6, 100)
	if err != nil {
		t.Fatalf("ReadLogRange failed: %v", err)
	}
	// Should return "world" (5 bytes from position 6 to 11).
	if string(data) != "world" {
		t.Errorf("got %q, want %q", string(data), "world")
	}
}

func TestReadLogRange_NegativeOffset(t *testing.T) {
	// Create a temporary log file.
	tmpDir := t.TempDir()
	sessionID := "test-session"
	logPath := LogFilePath(tmpDir, sessionID)

	content := []byte("hello world")
	if err := os.WriteFile(logPath, content, 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	// Request with negative offset — should be treated as 0.
	data, err := ReadLogRange(tmpDir, sessionID, -10, 5)
	if err != nil {
		t.Fatalf("ReadLogRange failed: %v", err)
	}
	// Should return "hello" (first 5 bytes).
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", string(data), "hello")
	}
}

func TestReadLogRange_LegacyLogFile(t *testing.T) {
	// Create a temporary directory with legacy .jsonl file.
	tmpDir := t.TempDir()
	sessionID := "test-session"
	legacyPath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := []byte("legacy log content")
	if err := os.WriteFile(legacyPath, content, 0o644); err != nil {
		t.Fatalf("failed to write legacy log file: %v", err)
	}

	// ReadLogRange should find and read the legacy file.
	data, err := ReadLogRange(tmpDir, sessionID, 0, int64(len(content)))
	if err != nil {
		t.Fatalf("ReadLogRange failed: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("got %q, want %q", string(data), string(content))
	}
}
