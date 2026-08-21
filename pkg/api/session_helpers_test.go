package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablememory "github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/runtime"
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

func TestResolveResumeSessionUsesDurableProviderIdentityAndRootVolumeAfterRestart(t *testing.T) {
	store := durablememory.New()
	ctx := context.Background()
	createDurableResumeFixture(t, store, "root-session", json.RawMessage(`{"agent":"codex","runtime":"docker","persist_session":true}`), "provider-thread")
	createDurableResumeFixture(t, store, "followup-session", json.RawMessage(`{"agent":"codex","runtime":"docker","persist_session":true,"resume_session":"root-session"}`), "provider-thread")
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DurableStore: store})

	resolved, err := server.resolveResumeSession(ctx, "codex", "followup-session", nil)
	if err != nil {
		t.Fatalf("resolve durable continuation: %v", err)
	}
	if resolved.ProviderID != "provider-thread" || resolved.VolumeName != "agentruntime-vol-root-session" || resolved.SourceSessionID != "followup-session" {
		t.Fatalf("durable continuation = %+v", resolved)
	}
}

func TestDockerNativeSessionsPersistProviderStateByDefaultExceptEphemeralPolicy(t *testing.T) {
	unrestricted := SessionRequest{Agent: "codex"}
	configureDockerProviderPersistence(&unrestricted, "docker")
	if !unrestricted.PersistSession {
		t.Fatal("unrestricted Docker Codex session did not enable provider persistence")
	}
	restricted := SessionRequest{Agent: "codex", ExecutionPolicy: &ExecutionPolicy{Version: ExecutionPolicyVersion}}
	configureDockerProviderPersistence(&restricted, "docker")
	if restricted.PersistSession {
		t.Fatal("ephemeral execution-policy session must not retain provider state")
	}
}

func TestPlanProviderVolumeCreatesFirstGenerationAndReusesContinuation(t *testing.T) {
	first := planProviderVolume("first-session", true)
	if first.Name != "agentruntime-vol-first-session" || first.ExistingName != "" {
		t.Fatalf("first-generation provider volume plan = %+v", first)
	}

	continuation := planProviderVolume("followup-session", true, "agentruntime-vol-first-session")
	if continuation.Name != "agentruntime-vol-first-session" || continuation.ExistingName != continuation.Name {
		t.Fatalf("continuation provider volume plan = %+v", continuation)
	}

	ephemeral := planProviderVolume("ephemeral-session", false, "agentruntime-vol-ignored")
	if ephemeral.Name != "" || ephemeral.ExistingName != "" {
		t.Fatalf("ephemeral provider volume plan = %+v", ephemeral)
	}
}

func TestDockerResumeRejectsProviderIDWithoutDurableVolumeLineage(t *testing.T) {
	resolved := resolvedResumeSession{ProviderID: "provider-only-id"}
	if err := validateResolvedResumeState("docker", "provider-only-id", resolved); err == nil {
		t.Fatal("Docker provider-ID-only resume did not fail closed")
	}
	if err := validateResolvedResumeState("local", "provider-only-id", resolved); err != nil {
		t.Fatalf("local provider-ID resume was rejected: %v", err)
	}
}

func createDurableResumeFixture(t *testing.T, store durable.Store, sessionID string, manifest json.RawMessage, providerID string) {
	t.Helper()
	ctx := context.Background()
	created, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: sessionID, IdempotencyKey: "job-" + sessionID, RequestHash: "sha256:" + sessionID,
		RequestManifest: manifest, Agent: "codex", Runtime: "docker", CreatedAt: time.Now().UTC(),
	})
	if err != nil || !created.Created {
		t.Fatalf("create fixture session: created=%v err=%v", created.Created, err)
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: sessionID, Runtime: "docker", ContainerID: "container-" + sessionID,
		ProviderID: providerID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create fixture generation: %v", err)
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
