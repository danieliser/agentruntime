package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogFilePath(t *testing.T) {
	got := LogFilePath("/var/log", "abc-123")
	want := filepath.Join("/var/log", "abc-123.ndjson")
	if got != want {
		t.Errorf("LogFilePath = %q, want %q", got, want)
	}
}

func TestNewLogWriter_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	lw, err := NewLogWriter(dir, "sess-1")
	if err != nil {
		t.Fatalf("NewLogWriter: %v", err)
	}
	defer lw.Close()

	if lw.Path() != LogFilePath(dir, "sess-1") {
		t.Errorf("Path = %q, want %q", lw.Path(), LogFilePath(dir, "sess-1"))
	}

	// File should exist on disk.
	if _, err := os.Stat(lw.Path()); err != nil {
		t.Errorf("log file not on disk: %v", err)
	}
}

func TestNewLogWriter_CreatesParentDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deep")
	lw, err := NewLogWriter(dir, "sess-2")
	if err != nil {
		t.Fatalf("NewLogWriter: %v", err)
	}
	defer lw.Close()

	if _, err := os.Stat(lw.Path()); err != nil {
		t.Errorf("log file not created in nested dir: %v", err)
	}
}

func TestLogWriter_WriteAndClose(t *testing.T) {
	dir := t.TempDir()
	lw, err := NewLogWriter(dir, "sess-3")
	if err != nil {
		t.Fatalf("NewLogWriter: %v", err)
	}

	data := []byte(`{"type":"event"}` + "\n")
	n, err := lw.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write n = %d, want %d", n, len(data))
	}

	if err := lw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify content.
	got, err := os.ReadFile(lw.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("file content = %q, want %q", got, data)
	}
}

func TestLogWriter_CloseNil(t *testing.T) {
	lw := &LogWriter{}
	if err := lw.Close(); err != nil {
		t.Errorf("Close on nil file should not error, got: %v", err)
	}
}

func TestExistingLogFilePath_CurrentExt(t *testing.T) {
	dir := t.TempDir()
	// Create a .ndjson file.
	path := filepath.Join(dir, "sess-4.ndjson")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found, err := ExistingLogFilePath(dir, "sess-4")
	if err != nil {
		t.Fatalf("ExistingLogFilePath: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for .ndjson file")
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
}

func TestExistingLogFilePath_LegacyExt(t *testing.T) {
	dir := t.TempDir()
	// Create only a .jsonl file (legacy).
	path := filepath.Join(dir, "sess-5.jsonl")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found, err := ExistingLogFilePath(dir, "sess-5")
	if err != nil {
		t.Fatalf("ExistingLogFilePath: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for legacy .jsonl file")
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
}

func TestExistingLogFilePath_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, found, err := ExistingLogFilePath(dir, "nonexistent")
	if err != nil {
		t.Fatalf("ExistingLogFilePath: %v", err)
	}
	if found {
		t.Error("expected found=false for missing file")
	}
}

func TestExistingLogFilePath_PrefersNdjson(t *testing.T) {
	dir := t.TempDir()
	// Create both extensions — should prefer .ndjson.
	ndjson := filepath.Join(dir, "sess-6.ndjson")
	jsonl := filepath.Join(dir, "sess-6.jsonl")
	os.WriteFile(ndjson, []byte("{}"), 0o644)
	os.WriteFile(jsonl, []byte("{}"), 0o644)

	got, found, err := ExistingLogFilePath(dir, "sess-6")
	if err != nil {
		t.Fatalf("ExistingLogFilePath: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got != ndjson {
		t.Errorf("should prefer .ndjson, got %q", got)
	}
}

func TestDrainWriter_NilLog(t *testing.T) {
	replay := NewReplayBuffer(1024)
	w := DrainWriter(replay, nil)
	if w != replay {
		t.Error("DrainWriter with nil logw should return replay directly")
	}
}

func TestDrainWriter_WithLog(t *testing.T) {
	dir := t.TempDir()
	replay := NewReplayBuffer(1024)
	lw, err := NewLogWriter(dir, "sess-drain")
	if err != nil {
		t.Fatalf("NewLogWriter: %v", err)
	}
	defer lw.Close()

	w := DrainWriter(replay, lw)
	data := []byte("test-line\n")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write n = %d, want %d", n, len(data))
	}

	// Both replay and log should have the data.
	if replay.TotalBytes() != int64(len(data)) {
		t.Errorf("replay bytes = %d, want %d", replay.TotalBytes(), len(data))
	}
}
