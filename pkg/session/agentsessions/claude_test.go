package agentsessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMangleProjectPath(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"/Users/me/project", "-Users-me-project"},
		{"/private/tmp", "-private-tmp"},
		{"/", "-"},
		{"/a/b/c/d", "-a-b-c-d"},
	}
	for _, tc := range cases {
		got := MangleProjectPath(tc.input)
		if got != tc.want {
			t.Errorf("MangleProjectPath(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestInitClaudeSessionDir_CreatesStructure(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir, err := InitClaudeSessionDir(dataDir, "sess-001", "/workspace/myproject", "")
	if err != nil {
		t.Fatalf("InitClaudeSessionDir: %v", err)
	}

	// Session dir exists.
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("session dir missing: %v", err)
	}

	// Projects subdir exists with mangled path.
	projectDir := filepath.Join(sessionDir, "projects", "-workspace-myproject")
	if _, err := os.Stat(projectDir); err != nil {
		t.Fatalf("project dir missing: %v", err)
	}

	// Sessions index dir exists.
	sessionsDir := filepath.Join(sessionDir, "sessions")
	if _, err := os.Stat(sessionsDir); err != nil {
		t.Fatalf("sessions dir missing: %v", err)
	}
}

func TestInitClaudeSessionDir_CopiesCredentials(t *testing.T) {
	dataDir := t.TempDir()

	// Write fake credentials.
	credsFile := filepath.Join(t.TempDir(), "creds.json")
	os.WriteFile(credsFile, []byte(`{"token":"test-oauth"}`), 0o600)

	sessionDir, err := InitClaudeSessionDir(dataDir, "sess-creds", "/workspace", credsFile)
	if err != nil {
		t.Fatalf("InitClaudeSessionDir: %v", err)
	}

	// Credentials should be copied into session dir under both names.
	for _, copied := range []string{
		filepath.Join(sessionDir, "credentials.json"),
		filepath.Join(sessionDir, ".credentials.json"),
	} {
		data, err := os.ReadFile(copied)
		if err != nil {
			t.Fatalf("credentials not copied: %v", err)
		}
		if string(data) != `{"token":"test-oauth"}` {
			t.Fatalf("credentials content mismatch: %q", data)
		}
	}
}

func TestInitClaudeSessionDir_MissingCredsSilentlySkipped(t *testing.T) {
	dataDir := t.TempDir()
	_, err := InitClaudeSessionDir(dataDir, "sess-nocreds", "/workspace", "/nonexistent/creds.json")
	if err != nil {
		t.Fatalf("expected no error for missing creds, got: %v", err)
	}
}

func TestReadLastClaudeSessionID_FromSessionsIndex(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir, _ := InitClaudeSessionDir(dataDir, "sess-read", "/workspace", "")

	// Write two session entries (PID-based).
	sessionsDir := filepath.Join(sessionDir, "sessions")
	writeJSON(t, filepath.Join(sessionsDir, "1000.json"), claudeSessionEntry{
		PID: 1000, SessionID: "old-session", CWD: "/workspace", StartedAt: 100,
	})
	writeJSON(t, filepath.Join(sessionsDir, "2000.json"), claudeSessionEntry{
		PID: 2000, SessionID: "new-session", CWD: "/workspace", StartedAt: 200,
	})

	got, err := ReadLastClaudeSessionID(sessionDir)
	if err != nil {
		t.Fatalf("ReadLastClaudeSessionID: %v", err)
	}
	if got != "new-session" {
		t.Fatalf("expected 'new-session', got %q", got)
	}
}

func TestReadLastClaudeSessionID_FallbackToJsonlMtime(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir, _ := InitClaudeSessionDir(dataDir, "sess-mtime", "/workspace", "")

	// Write a .jsonl file (no sessions index).
	projectDir := filepath.Join(sessionDir, "projects", "-workspace")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, "abc-123.jsonl"), []byte("{}"), 0o644)

	got, err := ReadLastClaudeSessionID(sessionDir)
	if err != nil {
		t.Fatalf("ReadLastClaudeSessionID: %v", err)
	}
	if got != "abc-123" {
		t.Fatalf("expected 'abc-123', got %q", got)
	}
}

func TestReadLastClaudeSessionID_EmptyDir(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir, _ := InitClaudeSessionDir(dataDir, "sess-empty", "/workspace", "")

	got, err := ReadLastClaudeSessionID(sessionDir)
	if err != nil {
		t.Fatalf("ReadLastClaudeSessionID: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string for no sessions, got %q", got)
	}
}

func TestClaudeResumeArgs_WithPriorSession(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir, _ := InitClaudeSessionDir(dataDir, "sess-resume", "/workspace", "")

	sessionsDir := filepath.Join(sessionDir, "sessions")
	writeJSON(t, filepath.Join(sessionsDir, "5000.json"), claudeSessionEntry{
		PID: 5000, SessionID: "resume-me", CWD: "/workspace", StartedAt: 300,
	})

	args, err := ClaudeResumeArgs(dataDir, "sess-resume")
	if err != nil {
		t.Fatalf("ClaudeResumeArgs: %v", err)
	}
	if len(args) != 3 || args[0] != "--resume" || args[2] != "resume-me" {
		t.Fatalf("expected [--resume --session-id resume-me], got %v", args)
	}
}

func TestClaudeResumeArgs_NoPriorSession(t *testing.T) {
	dataDir := t.TempDir()
	InitClaudeSessionDir(dataDir, "sess-first", "/workspace", "")

	args, err := ClaudeResumeArgs(dataDir, "sess-first")
	if err != nil {
		t.Fatalf("ClaudeResumeArgs: %v", err)
	}
	if args != nil {
		t.Fatalf("expected nil args for first run, got %v", args)
	}
}

func TestInitClaudeSessionDir_IdempotentSecondCall(t *testing.T) {
	dataDir := t.TempDir()
	dir1, err := InitClaudeSessionDir(dataDir, "sess-idem", "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	dir2, err := InitClaudeSessionDir(dataDir, "sess-idem", "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	if dir1 != dir2 {
		t.Fatalf("expected same dir on second call, got %q vs %q", dir1, dir2)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
