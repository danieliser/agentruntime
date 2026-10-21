package runtime

import (
	"os"
	"path/filepath"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestRestrictedLocalStartupCleanupDoesNotFollowLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "local-home"), 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "local-home", "credentials.json")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	sessions := filepath.Join(data, "claude-sessions")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sessions, "linked-session")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "codex-sessions")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(sessions, "real-session")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	durableCredential := filepath.Join(state, "credentials.json")
	if err := os.WriteFile(durableCredential, []byte("durable Docker credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "local-home"), filepath.Join(state, "local-home")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := CleanupRestrictedLocalHomes(data); err != nil {
			t.Fatal(err)
		}
	}
	if bytes, err := os.ReadFile(keep); err != nil || string(bytes) != "keep" {
		t.Fatal("cleanup followed a symlink", err)
	}
	if _, err := os.Lstat(filepath.Join(state, "local-home")); !os.IsNotExist(err) {
		t.Fatal("orphaned home link retained", err)
	}
	if data, err := os.ReadFile(durableCredential); err != nil || string(data) != "durable Docker credential" {
		t.Fatal("cleanup removed durable provider state", err)
	}
}

func TestRestrictedLocalPrecleanRejectsLinkedStorage(t *testing.T) {
	for _, linkRoot := range []bool{false, true} {
		root := t.TempDir()
		work, data, outside := filepath.Join(root, "work"), filepath.Join(root, "data"), filepath.Join(root, "outside")
		providerRoot := filepath.Join(data, "claude-sessions")
		for _, dir := range []string{work, data, outside} {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		target, link := outside, filepath.Join(providerRoot, "fixture")
		if linkRoot {
			link = providerRoot
			target = filepath.Join(outside, "fixture")
		} else if err := os.MkdirAll(providerRoot, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(target, "local-home"), 0700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(target, "local-home", "credential")
		if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		req := &apischema.SessionRequest{Agent: "claude", Runtime: "local", WorkDir: work}
		_, _, _, err := materializeRestrictedLocal(SpawnConfig{SessionID: "fixture", DataDir: data, WorkDir: work, Request: req})
		if err == nil {
			t.Fatal("linked session storage accepted")
		}
		if content, err := os.ReadFile(keep); err != nil || string(content) != "keep" {
			t.Fatal("preclean traversed a storage symlink", err)
		}
	}
}
