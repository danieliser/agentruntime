package credentials

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockExtractor simulates a credential store.
type mockExtractor struct {
	result string
	err    error
	calls  int
}

func (m *mockExtractor) Extract(service string) (string, error) {
	m.calls++
	return m.result, m.err
}

func TestClaudeCredentialsFile_ExtractsAndCaches(t *testing.T) {
	dataDir := t.TempDir()
	mock := &mockExtractor{result: `{"claudeAiOauth":{"accessToken":"test-token"}}`}
	s := newSyncWithExtractor(dataDir, mock)

	path, err := s.ClaudeCredentialsFile()
	if err != nil {
		t.Fatalf("ClaudeCredentialsFile: %v", err)
	}

	// File should exist at the returned path.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if string(data) != mock.result {
		t.Fatalf("cached content mismatch: %q", data)
	}

	// Should have called extractor once.
	if mock.calls != 1 {
		t.Fatalf("expected 1 extraction call, got %d", mock.calls)
	}
}

func TestClaudeCredentialsFile_ThrottlesRefresh(t *testing.T) {
	dataDir := t.TempDir()
	mock := &mockExtractor{result: `{"token":"v1"}`}
	s := newSyncWithExtractor(dataDir, mock)

	// First call extracts.
	s.ClaudeCredentialsFile()
	if mock.calls != 1 {
		t.Fatalf("expected 1 call after first, got %d", mock.calls)
	}

	// Second call within 30s should not re-extract.
	s.ClaudeCredentialsFile()
	if mock.calls != 1 {
		t.Fatalf("expected 1 call after second (throttled), got %d", mock.calls)
	}
}

func TestClaudeCredentialsFile_RefreshesWhenStale(t *testing.T) {
	dataDir := t.TempDir()
	mock := &mockExtractor{result: `{"token":"v1"}`}
	s := newSyncWithExtractor(dataDir, mock)

	// First call.
	path, _ := s.ClaudeCredentialsFile()
	if mock.calls != 1 {
		t.Fatalf("expected 1 call, got %d", mock.calls)
	}

	// Make the cache look old.
	oldTime := time.Now().Add(-60 * time.Second)
	os.Chtimes(path, oldTime, oldTime)

	// Update the mock to return new data.
	mock.result = `{"token":"v2"}`

	// Should re-extract.
	path2, _ := s.ClaudeCredentialsFile()
	if mock.calls != 2 {
		t.Fatalf("expected 2 calls after stale, got %d", mock.calls)
	}

	data, _ := os.ReadFile(path2)
	if string(data) != `{"token":"v2"}` {
		t.Fatalf("expected refreshed content, got %q", data)
	}
}

func TestClaudeCredentialsFile_FilePermissions(t *testing.T) {
	dataDir := t.TempDir()
	mock := &mockExtractor{result: `{"token":"secret"}`}
	s := newSyncWithExtractor(dataDir, mock)

	path, _ := s.ClaudeCredentialsFile()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", info.Mode().Perm())
	}
}

func TestClaudeCredentialsFile_ReturnsStaleCacheOnExtractionFailure(t *testing.T) {
	dataDir := t.TempDir()

	// Write a stale cache manually.
	cacheDir := filepath.Join(dataDir, credentialsSubdir)
	os.MkdirAll(cacheDir, 0o755)
	cachePath := filepath.Join(cacheDir, claudeCacheFile)
	os.WriteFile(cachePath, []byte(`{"stale":"cache"}`), 0o600)

	// Make it old so it would normally refresh.
	oldTime := time.Now().Add(-60 * time.Second)
	os.Chtimes(cachePath, oldTime, oldTime)

	// Mock extractor fails.
	mock := &mockExtractor{err: os.ErrNotExist}
	s := newSyncWithExtractor(dataDir, mock)

	// Should return stale cache instead of error.
	path, err := s.ClaudeCredentialsFile()
	if err != nil {
		t.Fatalf("expected stale cache fallback, got error: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != `{"stale":"cache"}` {
		t.Fatalf("expected stale cache content, got %q", data)
	}
}

func TestCodexAPIKey_PrefersEnvVar(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "env-key-123")
	mock := &mockExtractor{result: "keychain-key"}
	s := newSyncWithExtractor(t.TempDir(), mock)

	key, err := s.CodexAPIKey()
	if err != nil {
		t.Fatalf("CodexAPIKey: %v", err)
	}
	if key != "env-key-123" {
		t.Fatalf("expected env key, got %q", key)
	}
	if mock.calls != 0 {
		t.Fatal("should not have called extractor when env var is set")
	}
}

func TestCodexAPIKey_FallsBackToKeychain(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	mock := &mockExtractor{result: "keychain-key"}
	s := newSyncWithExtractor(t.TempDir(), mock)

	key, err := s.CodexAPIKey()
	if err != nil {
		t.Fatalf("CodexAPIKey: %v", err)
	}
	if key != "keychain-key" {
		t.Fatalf("expected keychain key, got %q", key)
	}
}
