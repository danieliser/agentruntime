package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func TestMigrationV2AllowsOneWayProviderIdentityBinding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentd.sqlite")
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatalf("open v1 fixture: %v", err)
	}
	migration, err := migrations.ReadFile("migrations/001_durable_store_v1.sql")
	if err != nil {
		t.Fatalf("read v1 migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply v1 migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set v1 schema: %v", err)
	}
	legacy := &Store{db: db, path: path}
	createTestHistory(t, ctx, legacy, "v1-provider-session", 0)
	if err := legacy.Close(); err != nil {
		t.Fatalf("close v1 fixture: %v", err)
	}

	upgraded := openTestStore(t, path)
	bound, err := upgraded.BindGenerationProvider(ctx, durable.BindGenerationProviderParams{
		SessionID: "v1-provider-session", Generation: 1, ProviderID: "provider-v2",
		At: time.Unix(105, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("bind provider after v2 migration: %v", err)
	}
	if bound.ProviderID != "provider-v2" {
		t.Fatalf("provider ID = %q, want provider-v2", bound.ProviderID)
	}
	_, err = upgraded.db.ExecContext(ctx, "UPDATE runtime_generations SET provider_id = 'changed' WHERE session_id = 'v1-provider-session'")
	if err == nil {
		t.Fatal("second provider identity mutation unexpectedly succeeded")
	}
}

func TestMigrationV3AddsTerminalReasonWithoutRewritingReceipts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentd.sqlite")
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatalf("open v2 fixture: %v", err)
	}
	for _, name := range []string{"migrations/001_durable_store_v1.sql", "migrations/002_durable_store_v2.sql"} {
		migration, readErr := migrations.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if _, execErr := db.ExecContext(ctx, string(migration)); execErr != nil {
			t.Fatalf("apply %s: %v", name, execErr)
		}
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO sessions (id, idempotency_key, request_hash, request_manifest_json, agent, runtime, state, active_generation, created_at_ns, updated_at_ns)
VALUES ('v2-terminal', 'job-v2-terminal', 'sha256:v2', '{}', 'claude', 'docker', 'completed', 1, 100, 300);
INSERT INTO runtime_generations (session_id, generation, runtime, state, container_id, image_reference, image_digest, sandbox_profile, created_at_ns, updated_at_ns)
VALUES ('v2-terminal', 1, 'docker', 'exited', 'container-v2-terminal', 'image:v2', 'sha256:v2', 'docker-native-v1', 200, 300);
INSERT INTO terminal_receipts (session_id, generation, state, exit_code, started_at_ns, ended_at_ns, output_hash, last_sequence)
VALUES ('v2-terminal', 1, 'completed', 0, 200, 300, 'sha256:output', 0);
PRAGMA user_version = 2;`); err != nil {
		t.Fatalf("seed v2 receipt: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v2 fixture: %v", err)
	}

	upgraded := openTestStore(t, path)
	receipt, err := upgraded.GetTerminalReceipt(ctx, "v2-terminal")
	if err != nil || receipt.Reason != "completed" {
		t.Fatalf("upgraded receipt = %+v err=%v", receipt, err)
	}
	if _, err := upgraded.db.ExecContext(ctx, "UPDATE terminal_receipts SET terminal_reason = 'terminated' WHERE session_id = 'v2-terminal'"); err == nil {
		t.Fatal("immutable v2 receipt reason unexpectedly changed after migration")
	}
}

func TestStoreSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentd.sqlite")
	store := openTestStore(t, path)
	createTestHistory(t, ctx, store, "restart-session", 2)
	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	reopened := openTestStore(t, path)
	session, err := reopened.GetSessionByIdempotencyKey(ctx, "job-restart-session")
	if err != nil {
		t.Fatalf("lookup after restart: %v", err)
	}
	if session.ID != "restart-session" || session.ActiveGeneration != 1 || session.LastSequence != 2 {
		t.Fatalf("reconstructed session = %+v", session)
	}
	generation, err := reopened.GetGeneration(ctx, session.ID, 1)
	if err != nil {
		t.Fatalf("generation after restart: %v", err)
	}
	if generation.ContainerID != "container-restart-session" || generation.DockerLogDriver != "local" {
		t.Fatalf("reconstructed generation = %+v", generation)
	}
	page, err := reopened.ListEvents(ctx, durable.EventQuery{SessionID: session.ID})
	if err != nil {
		t.Fatalf("events after restart: %v", err)
	}
	if len(page.Events) != 2 || page.Events[0].EventID != "restart-session-event-1" || page.Events[1].Sequence != 2 {
		t.Fatalf("reconstructed event page = %+v", page)
	}
}

func TestTerminalReceiptSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentd.sqlite")
	store := openTestStore(t, path)
	createTestHistory(t, ctx, store, "terminal-session", 1)
	for _, transition := range []durable.TransitionSessionParams{
		{SessionID: "terminal-session", From: durable.StateCreated, To: durable.StateStarting, At: time.Unix(110, 0).UTC()},
		{SessionID: "terminal-session", From: durable.StateStarting, To: durable.StateRunning, At: time.Unix(111, 0).UTC()},
	} {
		if _, err := store.TransitionSession(ctx, transition); err != nil {
			t.Fatalf("transition session: %v", err)
		}
	}
	if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{
		SessionID: "terminal-session", Generation: 1, From: durable.GenerationStarting,
		To: durable.GenerationRunning, At: time.Unix(111, 0).UTC(),
	}); err != nil {
		t.Fatalf("transition generation: %v", err)
	}
	exitCode := 0
	receipt := durable.TerminalReceipt{
		SessionID: "terminal-session", Generation: 1, State: durable.StateCompleted, Reason: string(durable.StateCompleted),
		ExitCode: &exitCode, StartedAt: time.Unix(111, 0).UTC(), EndedAt: time.Unix(112, 0).UTC(),
		OutputHash: "sha256:output", ArtifactHash: "sha256:artifacts", LastSequence: 1,
	}
	if _, err := store.FinalizeSession(ctx, durable.FinalizeSessionParams{
		From: durable.StateRunning, GenerationFrom: durable.GenerationRunning,
		GenerationTo: durable.GenerationExited, Receipt: receipt,
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	reopened := openTestStore(t, path)
	got, err := reopened.GetTerminalReceipt(ctx, "terminal-session")
	if err != nil {
		t.Fatalf("receipt after restart: %v", err)
	}
	if !receiptsEqual(got, receipt) {
		t.Fatalf("receipt after restart = %+v, want %+v", got, receipt)
	}
	session, err := reopened.GetSession(ctx, "terminal-session")
	if err != nil {
		t.Fatalf("terminal session after restart: %v", err)
	}
	if session.State != durable.StateCompleted {
		t.Fatalf("session state after restart = %q, want completed", session.State)
	}
}

func TestCheckIntegrityDetectsRemovedEvent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "agentd.sqlite"))
	createTestHistory(t, ctx, store, "gap-session", 2)

	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER events_no_delete"); err != nil {
		t.Fatalf("disable append-only trigger for corruption fixture: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM events WHERE session_id = ? AND sequence = 1", "gap-session"); err != nil {
		t.Fatalf("remove event for corruption fixture: %v", err)
	}

	if err := store.CheckIntegrity(ctx); !durable.IsCode(err, durable.CodeEventGap) {
		t.Fatalf("integrity error = %v, want %s", err, durable.CodeEventGap)
	}
	_, err := store.ListEvents(ctx, durable.EventQuery{SessionID: "gap-session"})
	if !durable.IsCode(err, durable.CodeEventGap) {
		t.Fatalf("replay error = %v, want %s", err, durable.CodeEventGap)
	}
}

func TestCheckIntegrityDetectsMutatedRawEvent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "agentd.sqlite"))
	createTestHistory(t, ctx, store, "hash-session", 1)
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER events_no_update"); err != nil {
		t.Fatalf("disable append-only trigger for corruption fixture: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE events SET raw = ? WHERE session_id = ?", []byte(`{"mutated":true}`), "hash-session"); err != nil {
		t.Fatalf("mutate raw event for corruption fixture: %v", err)
	}
	if err := store.CheckIntegrity(ctx); !durable.IsCode(err, durable.CodeIndeterminate) {
		t.Fatalf("integrity error = %v, want %s", err, durable.CodeIndeterminate)
	}
}

func TestMigrationTriggersRejectImmutableMutation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "agentd.sqlite"))
	createTestHistory(t, ctx, store, "trigger-session", 1)
	for name, statement := range map[string]string{
		"event update":        "UPDATE events SET raw = X'00' WHERE session_id = 'trigger-session'",
		"event delete":        "DELETE FROM events WHERE session_id = 'trigger-session'",
		"session identity":    "UPDATE sessions SET request_hash = 'changed' WHERE id = 'trigger-session'",
		"generation identity": "UPDATE runtime_generations SET container_id = 'changed' WHERE session_id = 'trigger-session'",
	} {
		if _, err := store.db.ExecContext(ctx, statement); err == nil {
			t.Errorf("%s unexpectedly succeeded", name)
		}
	}
}

func TestBackupRestoresConsistentHistory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := openTestStore(t, filepath.Join(root, "agentd.sqlite"))
	createTestHistory(t, ctx, store, "backup-session", 3)
	backupPath := filepath.Join(root, "backups", "snapshot.sqlite")

	if err := store.Backup(ctx, backupPath); err != nil {
		t.Fatalf("backup: %v", err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup mode = %#o, want 0600", got)
	}
	metadataBytes, err := os.ReadFile(backupPath + ".metadata.json")
	if err != nil {
		t.Fatalf("read backup metadata: %v", err)
	}
	metadataInfo, err := os.Stat(backupPath + ".metadata.json")
	if err != nil {
		t.Fatalf("stat backup metadata: %v", err)
	}
	if got := metadataInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup metadata mode = %#o, want 0600", got)
	}
	var metadata struct {
		SchemaVersion  int    `json:"schema_version"`
		DatabaseSHA256 string `json:"database_sha256"`
		SessionTails   []struct {
			SessionID    string `json:"session_id"`
			LastSequence int64  `json:"last_sequence"`
		} `json:"session_tails"`
	}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatalf("decode backup metadata: %v", err)
	}
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup for hash: %v", err)
	}
	digest := sha256.Sum256(backupBytes)
	if metadata.SchemaVersion != 3 || metadata.DatabaseSHA256 != fmt.Sprintf("sha256:%x", digest) {
		t.Fatalf("backup metadata = %+v", metadata)
	}
	if len(metadata.SessionTails) != 1 || metadata.SessionTails[0].SessionID != "backup-session" || metadata.SessionTails[0].LastSequence != 3 {
		t.Fatalf("backup session tails = %+v", metadata.SessionTails)
	}
	restored := openTestStore(t, backupPath)
	if err := restored.CheckIntegrity(ctx); err != nil {
		t.Fatalf("restored integrity: %v", err)
	}
	page, err := restored.ListEvents(ctx, durable.EventQuery{SessionID: "backup-session"})
	if err != nil {
		t.Fatalf("restored events: %v", err)
	}
	if len(page.Events) != 3 || page.LastSequence != 3 {
		t.Fatalf("restored page = %+v", page)
	}
	if err := store.Backup(ctx, backupPath); !durable.IsCode(err, durable.CodeImmutableConflict) {
		t.Fatalf("overwrite backup error = %v, want %s", err, durable.CodeImmutableConflict)
	}
}

func TestStoreUsesPrivateFilesystemModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(root, "agentd.sqlite")
	store := openTestStore(t, path)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	for name, want := range map[string]os.FileMode{root: 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode %s = %#o, want %#o", name, got, want)
		}
	}
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func createTestHistory(t *testing.T, ctx context.Context, store *Store, sessionID string, eventCount int) {
	t.Helper()
	if _, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: sessionID, IdempotencyKey: "job-" + sessionID, RequestHash: "sha256:" + sessionID,
		RequestManifest: json.RawMessage(`{"agent":"claude","runtime":"docker"}`),
		Agent:           "claude", Runtime: "docker", CreatedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: sessionID, Runtime: "docker", ContainerID: "container-" + sessionID,
		ImageReference: "agent:fixture", ImageDigest: "sha256:image", SandboxProfile: "sandbox-v1",
		DockerLogDriver: "local", DockerLogOptions: json.RawMessage(`{"max-size":"10m"}`),
		CreatedAt: time.Unix(101, 0).UTC(),
	}); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	for sequence := 1; sequence <= eventCount; sequence++ {
		raw := []byte(`{"text":"hello"}`)
		if _, err := store.AppendEvent(ctx, durable.AppendEventParams{
			SchemaVersion: "1.0", EventID: sessionID + "-event-" + string(rune('0'+sequence)),
			SessionID: sessionID, Generation: 1, Timestamp: time.Unix(102+int64(sequence), 0).UTC(),
			Type: "content.delta", Stream: durable.StreamProviderStdout,
			Payload: json.RawMessage(raw), Raw: raw,
		}); err != nil {
			t.Fatalf("append event %d: %v", sequence, err)
		}
	}
}
