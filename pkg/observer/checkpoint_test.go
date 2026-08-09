package observer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointStoreRoundTripAndMonotonicAdvance(t *testing.T) {
	root := t.TempDir()
	store, err := NewCheckpointStore(root, "opentraces")
	if err != nil {
		t.Fatal(err)
	}
	want := Checkpoint{SessionID: "718258fe-2921-4f67-91c9-cb70720264b4", Sequence: 9, EventID: "event-9", TraceID: "851ad0da-3f90-4ea8-9094-9b644d1913f7"}
	if err := store.Advance(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(want.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("checkpoint = %+v, want %+v", got, want)
	}
	if err := store.Advance(Checkpoint{SessionID: want.SessionID, Sequence: 8, EventID: "event-8"}); err == nil {
		t.Fatal("expected checkpoint regression to fail")
	}
	changedTrace := want
	changedTrace.TraceID = "c5415f44-74ef-45e1-836e-58ecaf405224"
	if err := store.Advance(changedTrace); err == nil {
		t.Fatal("expected trace linkage mutation to fail")
	}
	info, err := os.Stat(filepath.Join(root, "plugins", "opentraces", "checkpoints"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("checkpoint directory mode = %o, want private", info.Mode().Perm())
	}
}

func TestCheckpointStoreRejectsUnsafeNamesAndCorruption(t *testing.T) {
	if _, err := NewCheckpointStore(t.TempDir(), "../escape"); err == nil {
		t.Fatal("expected unsafe plugin name rejection")
	}
	root := t.TempDir()
	store, err := NewCheckpointStore(root, "opentraces")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "718258fe-2921-4f67-91c9-cb70720264b4"
	path := store.path(sessionID)
	if err := os.WriteFile(path, []byte(`{"session_id":"wrong","sequence":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(sessionID); err == nil {
		t.Fatal("expected corrupt/mismatched checkpoint rejection")
	}
}
