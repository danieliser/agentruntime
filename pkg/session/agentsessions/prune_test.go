package agentsessions

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneOldSessions_DeletesExpired(t *testing.T) {
	dataDir := t.TempDir()

	// Create two Claude sessions.
	InitClaudeSessionDir(dataDir, "old-session", "/workspace", "")
	InitClaudeSessionDir(dataDir, "new-session", "/workspace", "")

	// Make old-session look old.
	oldDir := filepath.Join(dataDir, "claude-sessions", "old-session")
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(oldDir, oldTime, oldTime)

	// Prune with 24h retention.
	err := PruneOldSessions(dataDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOldSessions: %v", err)
	}

	// Old should be gone.
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("expected old-session to be pruned")
	}

	// New should survive.
	newDir := filepath.Join(dataDir, "claude-sessions", "new-session")
	if _, err := os.Stat(newDir); err != nil {
		t.Fatal("expected new-session to survive pruning")
	}
}

func TestPruneOldSessions_EmptyDataDir(t *testing.T) {
	dataDir := t.TempDir()
	// Should not error on empty dir.
	err := PruneOldSessions(dataDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOldSessions on empty dir: %v", err)
	}
}

func TestPruneOldSessions_PrunesCodexToo(t *testing.T) {
	dataDir := t.TempDir()

	InitCodexSessionDir(dataDir, "old-codex")
	oldDir := filepath.Join(dataDir, "codex-sessions", "old-codex")
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(oldDir, oldTime, oldTime)

	err := PruneOldSessions(dataDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOldSessions: %v", err)
	}

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("expected old codex session to be pruned")
	}
}
