package skills

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStorePutAndExtract(t *testing.T) {
	store := NewStore(t.TempDir())
	bundle := testBundle(t, map[string]string{
		"persist-comms/SKILL.md":               "---\nname: persist-comms\n---\n",
		"persist-comms/references/protocol.md": "contract",
	})
	digest := digestOf(bundle)

	created, err := store.Put(digest, bytes.NewReader(bundle))
	if err != nil || !created {
		t.Fatalf("first Put = created %v, err %v", created, err)
	}
	created, err = store.Put(digest, bytes.NewReader(bundle))
	if err != nil || created {
		t.Fatalf("second Put = created %v, err %v", created, err)
	}
	dest := filepath.Join(t.TempDir(), "persist-comms")
	if err := store.Extract(digest, "persist-comms", dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "references", "protocol.md"))
	if err != nil || string(got) != "contract" {
		t.Fatalf("extracted resource = %q, err %v", got, err)
	}
}

func TestStoreRejectsUnsafeOrMismatchedBundles(t *testing.T) {
	store := NewStore(t.TempDir())
	valid := testBundle(t, map[string]string{"safe/SKILL.md": "safe"})
	if _, err := store.Put("sha256:"+fmt.Sprintf("%064d", 0), bytes.NewReader(valid)); err == nil {
		t.Fatal("expected digest mismatch")
	}
	unsafe := testBundle(t, map[string]string{"../escape/SKILL.md": "unsafe"})
	if _, err := store.Put(digestOf(unsafe), bytes.NewReader(unsafe)); err == nil {
		t.Fatal("expected traversal rejection")
	}
	link := new(bytes.Buffer)
	tw := tar.NewWriter(link)
	if err := tw.WriteHeader(&tar.Header{Name: "safe/SKILL.md", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(digestOf(link.Bytes()), bytes.NewReader(link.Bytes())); err == nil {
		t.Fatal("expected symlink rejection")
	}
	conflict := new(bytes.Buffer)
	conflictWriter := tar.NewWriter(conflict)
	for _, entry := range []struct {
		name string
		body string
	}{
		{"safe/SKILL.md", "safe"},
		{"safe/scripts", "not a directory"},
		{"safe/scripts/run.sh", "#!/bin/sh\n"},
	} {
		if err := conflictWriter.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o444, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := conflictWriter.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := conflictWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(digestOf(conflict.Bytes()), bytes.NewReader(conflict.Bytes())); err == nil {
		t.Fatal("expected file/directory path conflict rejection")
	}
}

func TestStoreExtractRejectsSymlinkedDestinationParent(t *testing.T) {
	store := NewStore(t.TempDir())
	bundle := testBundle(t, map[string]string{"persist-comms/SKILL.md": "safe"})
	digest := digestOf(bundle)
	if _, err := store.Put(digest, bytes.NewReader(bundle)); err != nil {
		t.Fatal(err)
	}
	sessionRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(sessionRoot, "skills")); err != nil {
		t.Fatal(err)
	}

	err := store.Extract(digest, "persist-comms", filepath.Join(sessionRoot, "skills", "persist-comms"))
	if err == nil {
		t.Fatal("expected symlinked destination parent to be rejected")
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "persist-comms")); !os.IsNotExist(statErr) {
		t.Fatalf("extraction escaped the session root: %v", statErr)
	}
}

func TestStoreExtractPreservesSafeExecutableBits(t *testing.T) {
	store := NewStore(t.TempDir())
	bundle := testExecutableBundle(t, "scripted")
	digest := digestOf(bundle)
	if _, err := store.Put(digest, bytes.NewReader(bundle)); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "scripted")
	if err := store.Extract(digest, "scripted", destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "scripts", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("extracted executable mode = %o, want 0555", info.Mode().Perm())
	}
}

func TestStoreEnsureRejectsOversizedStoredBundleWithoutReadingIt(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	digest := "sha256:" + strings.Repeat("0", 64)
	path := filepath.Join(root, strings.Repeat("0", 64)+".tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxArchiveBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(digest); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Ensure error = %v, want bounded-size rejection", err)
	}
}

func TestStoreExtractRechecksContentDigest(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	bundle := testBundle(t, map[string]string{"safe/SKILL.md": "original"})
	digest := digestOf(bundle)
	if _, err := store.Put(digest, bytes.NewReader(bundle)); err != nil {
		t.Fatal(err)
	}
	tampered := testBundle(t, map[string]string{"safe/SKILL.md": "tampered"})
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if err := os.WriteFile(filepath.Join(root, hexDigest+".tar"), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "safe")
	if err := store.Extract(digest, "safe", destination); err == nil {
		t.Fatal("expected extraction to reject a digest-mismatched CAS file")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("tampered bundle was materialized: %v", err)
	}
}

func TestStoreConcurrentPutPublishesExactlyOnce(t *testing.T) {
	store := NewStore(t.TempDir())
	bundle := testBundle(t, map[string]string{"safe/SKILL.md": "safe"})
	digest := digestOf(bundle)
	start := make(chan struct{})
	var created atomic.Int32
	errorsSeen := make(chan error, 32)
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			wasCreated, err := store.Put(digest, bytes.NewReader(bundle))
			if err != nil {
				errorsSeen <- err
				return
			}
			if wasCreated {
				created.Add(1)
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent Put: %v", err)
	}
	if got := created.Load(); got != 1 {
		t.Fatalf("created count = %d, want exactly 1", got)
	}
}

func testBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testExecutableBundle(t *testing.T, name string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	entries := []struct {
		path string
		mode int64
		body string
	}{
		{name + "/SKILL.md", 0o444, "skill"},
		{name + "/scripts/run.sh", 0o755, "#!/bin/sh\n"},
	}
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: entry.path, Mode: entry.mode, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}
