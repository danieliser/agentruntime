package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func TestMigrationV4PreservesReceiptsAndContinuesAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.sqlite")
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_durable_store_v1.sql", "002_durable_store_v2.sql", "003_durable_store_v3.sql"} {
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	legacy := &Store{db: db, path: path}
	createTestHistory(t, ctx, legacy, "upgraded", 1)
	for _, transition := range []durable.TransitionSessionParams{
		{SessionID: "upgraded", From: durable.StateCreated, To: durable.StateStarting, At: time.Unix(210, 0)},
		{SessionID: "upgraded", From: durable.StateStarting, To: durable.StateRunning, At: time.Unix(211, 0)},
	} {
		if _, err := legacy.TransitionSession(ctx, transition); err != nil {
			t.Fatal(err)
		}
	}
	first := durable.TerminalReceipt{SessionID: "upgraded", Generation: 1, State: durable.StateCompleted, Reason: "completed", StartedAt: time.Unix(211, 0), EndedAt: time.Unix(212, 0), OutputHash: "old-output", LastSequence: 1}
	if _, err := legacy.FinalizeSession(ctx, durable.FinalizeSessionParams{From: durable.StateRunning, GenerationFrom: durable.GenerationStarting, GenerationTo: durable.GenerationExited, Receipt: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	got, err := store.GetGenerationReceipt(ctx, "upgraded", 1)
	if err != nil || !receiptsEqual(got, first) {
		t.Fatalf("copied receipt=%+v err=%v", got, err)
	}
	var legacyRows int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM terminal_receipts_v3 WHERE session_id = 'upgraded'").Scan(&legacyRows); err != nil || legacyRows != 1 {
		t.Fatalf("legacy proof rows=%d err=%v", legacyRows, err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "upgraded", From: durable.StateCompleted, To: durable.StateStarting}); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatalf("integrity during wake: %v", err)
	}
	gen, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "upgraded", Runtime: "docker", ContainerID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "upgraded", From: durable.StateStarting, To: durable.StateRunning}); err != nil {
		t.Fatal(err)
	}
	second := durable.TerminalReceipt{SessionID: "upgraded", Generation: 2, State: durable.StateCompleted, Reason: "completed", StartedAt: gen.CreatedAt, EndedAt: time.Now(), OutputHash: "new-output", LastSequence: 1}
	if _, err := store.FinalizeSession(ctx, durable.FinalizeSessionParams{From: durable.StateRunning, GenerationFrom: durable.GenerationStarting, GenerationTo: durable.GenerationExited, Receipt: second}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	if err := reopened.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
	for _, want := range []durable.TerminalReceipt{first, second} {
		got, err := reopened.GetGenerationReceipt(ctx, "upgraded", want.Generation)
		if err != nil || !receiptsEqual(got, want) {
			t.Fatalf("restart receipt=%+v err=%v", got, err)
		}
	}
	for _, statement := range []string{"UPDATE terminal_receipts SET output_hash = 'changed'", "DELETE FROM terminal_receipts", "UPDATE terminal_receipts_v3 SET output_hash = 'changed'", "DELETE FROM terminal_receipts_v3", "UPDATE sessions SET state = 'running' WHERE id = 'upgraded'"} {
		if _, err := reopened.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("immutable mutation succeeded: %s", statement)
		}
	}
}
