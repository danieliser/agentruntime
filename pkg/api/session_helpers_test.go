package api

import (
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/danieliser/agentruntime/pkg/session/agentsessions"
)

func TestLookupResumeSessionIDPrefersTag(t *testing.T) {
	_, server := newTestServer(t)
	sess := session.NewSessionWithID("original", "", "claude", "test")
	sess.SetTag("claude_session_id", "provider-session")
	got, err := server.lookupResumeSessionID("claude", "original", sess)
	if err != nil || got != "provider-session" {
		t.Fatalf("resume ID = %q err=%v", got, err)
	}
}

func TestLookupResumeSessionIDFallsBackToFilesystem(t *testing.T) {
	_, server := newTestServer(t)
	sess := session.NewSessionWithID("original", "", "claude", "test")
	if _, err := agentsessions.InitClaudeSessionDir(server.dataDir, "original", "/workspace", ""); err != nil {
		t.Fatalf("initialize session dir: %v", err)
	}
	writeJSONFile(t, filepath.Join(server.dataDir, "claude-sessions", "original", "sessions", "100.json"), map[string]any{
		"pid": 100, "sessionId": "provider-filesystem", "cwd": "/workspace", "startedAt": 100,
	})
	got, err := server.lookupResumeSessionID("claude", "original", sess)
	if err != nil || got != "provider-filesystem" {
		t.Fatalf("resume ID = %q err=%v", got, err)
	}
}

func TestLookupResumeSessionIDPassesThroughProviderID(t *testing.T) {
	_, server := newTestServer(t)
	got, err := server.lookupResumeSessionID("claude", "provider-id", nil)
	if err != nil || got != "provider-id" {
		t.Fatalf("resume ID = %q err=%v", got, err)
	}
}

func TestEffectiveWorkDirIgnoresNamedVolumes(t *testing.T) {
	bind := t.TempDir()
	if got := effectiveWorkDir("", []Mount{{Host: "volume", Type: "volume", Mode: "rw"}}); got != "" {
		t.Fatalf("volume-only workdir = %q", got)
	}
	if got := effectiveWorkDir("", []Mount{{Host: "volume", Type: "volume", Mode: "rw"}, {Host: bind, Type: "bind", Mode: "rw"}}); got != bind {
		t.Fatalf("bind workdir = %q, want %q", got, bind)
	}
}
