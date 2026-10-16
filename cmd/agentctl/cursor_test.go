package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorPersistsPerCallerAndSessionWithPrivateAtomicFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cursors")
	cursor := CursorStore{Dir: dir}
	if got, err := cursor.Load("caller/one", "session:one"); err != nil || got != 0 {
		t.Fatalf("initial cursor = %d, %v", got, err)
	}
	if err := cursor.Save("caller/one", "session:one", 42); err != nil {
		t.Fatal(err)
	}
	if got, err := (CursorStore{Dir: dir}).Load("caller/one", "session:one"); err != nil || got != 42 {
		t.Fatalf("reloaded cursor = %d, %v", got, err)
	}
	if got, _ := cursor.Load("caller/two", "session:one"); got != 0 {
		t.Fatalf("caller cursor leaked: %d", got)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cursor directory mode = %v, err=%v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cursor entries = %v, err=%v", entries, err)
	}
	info, err = entries[0].Info()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cursor file mode = %v, err=%v", info, err)
	}
	if entries[0].Name() == "caller_one-session_one.json" {
		t.Fatal("unsafe ids used directly as filename")
	}
}
