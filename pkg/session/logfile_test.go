package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogFilePath(t *testing.T) {
	got := LogFilePath("/var/log", "abc-123")
	want := filepath.Join("/var/log", "abc-123.ndjson")
	if got != want {
		t.Errorf("LogFilePath = %q, want %q", got, want)
	}
}

func TestNewLogWriter_CreatesFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
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
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode = %v err=%v, want 0700", infoMode(info), err)
	}
	if info, err := os.Stat(lw.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %v err=%v, want 0600", infoMode(info), err)
	}
}

func TestNewLogWriterTightensExistingModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := LogFilePath(dir, "existing")
	if err := os.WriteFile(path, []byte("prior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writer, err := NewLogWriter(dir, "existing")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("existing directory mode = %o", info.Mode().Perm())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("existing file mode = %o", info.Mode().Perm())
	}
}

func TestLogWriterRedactsPromptsAndCredentialsAcrossWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	writer, err := NewLogWriter(dir, "redacted", WithDiagnosticRedactions(
		"private prompt", "sk-ant-secret-value", `{"access_token":"nested-secret"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{
		`{"type":"event","prompt":"private `,
		`prompt","api_key":"sk-ant-secret-value","auth":{"access_token":"nested-secret"}}` + "\n",
	} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private prompt", "sk-ant-secret-value", "nested-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("diagnostic log leaked %q: %s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("diagnostic log contains no redaction marker: %s", raw)
	}
}

func TestNewLogWriterDisabledDoesNotCreateAFile(t *testing.T) {
	writer, err := NewLogWriter("", "disabled")
	if err != nil || writer != nil {
		t.Fatalf("disabled writer = %#v err=%v", writer, err)
	}
	if path := LogFilePath("", "disabled"); path != "" {
		t.Fatalf("disabled log path = %q", path)
	}
}

func TestPruneDiagnosticLogsRemovesOnlyExpiredLogFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	old := filepath.Join(dir, "old.ndjson")
	recent := filepath.Join(dir, "recent.ndjson")
	unrelated := filepath.Join(dir, "keep.txt")
	for _, path := range []string{old, recent, unrelated} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recent, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneDiagnosticLogs(dir, 7*24*time.Hour, now)
	if err != nil || removed != 1 {
		t.Fatalf("prune removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired log remains: %v", err)
	}
	for _, path := range []string{recent, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained file %s: %v", path, err)
		}
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
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

func TestDrainWriterRedactsOnlyDiagnosticMirrorAndPreservesReplayBytes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	replay := NewReplayBuffer(1024)
	writer, err := NewLogWriter(dir, "redacted-drain", WithDiagnosticRedactions("private prompt"))
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("private prompt\n")
	if _, err := DrainWriter(replay, writer).Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	replayed, _ := replay.ReadFrom(0)
	if string(replayed) != string(raw) {
		t.Fatalf("canonical replay changed: %q", replayed)
	}
	logged, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "private prompt") || !strings.Contains(string(logged), "[REDACTED]") {
		t.Fatalf("diagnostic mirror was not independently redacted: %q", logged)
	}
}
