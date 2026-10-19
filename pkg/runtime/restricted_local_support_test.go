package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestrictedLocalQualificationCacheRefreshAndFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var fail atomic.Bool
	q := localQualificationCache{platform: "darwin", probe: func(ctx context.Context, binary, root string) (string, error) {
		calls.Add(1)
		if fail.Load() {
			return "fixture-v2", errors.New("confinement failed")
		}
		return "fixture-v1", nil
	}}
	q.root = t.TempDir()
	t.Cleanup(q.stop)
	var workers sync.WaitGroup
	for range 10 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if state := q.get(path).State; state != "pending" && state != "qualified" {
				t.Error("unexpected qualification state", state)
			}
		}()
	}
	workers.Wait()
	result := awaitQualificationResult(t, &q, path)
	if calls.Load() != 1 {
		t.Fatal("cached path performed repeated version/probe calls", calls.Load())
	}
	if result.Version != "fixture-v1" || result.CheckedAt.IsZero() {
		t.Fatal("missing measured evidence")
	}
	fail.Store(true)
	stamp := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if next := q.get(path); next.Qualified || next.State != "pending" {
		t.Fatal("changed binary retained stale qualification")
	}
	awaitQualificationResult(t, &q, path)
	if q.get(path).Qualified || calls.Load() != 2 {
		t.Fatal("failed qualification not cached")
	}
	if q.get(filepath.Join(t.TempDir(), "missing")).Qualified {
		t.Fatal("missing binary qualified")
	}
}

func TestRestrictedLocalQualificationFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	q := localQualificationCache{platform: "darwin", root: t.TempDir(), timeout: time.Millisecond, probe: func(ctx context.Context, _, _ string) (string, error) { <-ctx.Done(); return "fixture", ctx.Err() }}
	t.Cleanup(q.stop)
	if awaitQualificationResult(t, &q, path).Qualified {
		t.Fatal("timed out probe qualified")
	}
	unsupported := localQualificationCache{platform: "linux", root: t.TempDir(), probe: func(context.Context, string, string) (string, error) {
		t.Fatal("unsupported OS probed")
		return "", nil
	}}
	if unsupported.get(path).Qualified {
		t.Fatal("unsupported OS qualified")
	}
}

func TestRestrictedLocalQualificationTracksPathAndConcurrentReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "codex")
	if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	q := localQualificationCache{platform: "darwin", root: root, probe: func(context.Context, string, string) (string, error) { calls++; return "fixture", nil }}
	t.Cleanup(q.stop)
	if !awaitQualificationResult(t, &q, path).Qualified {
		t.Fatal("fixture unqualified")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if !awaitQualificationResult(t, &q, alias).Qualified || calls != 2 {
		t.Fatal("new binary lookup path did not trigger qualification")
	}
	q.mu.Lock()
	q.info = nil
	q.probe = func(context.Context, string, string) (string, error) {
		replacement := filepath.Join(root, "replacement")
		if err := os.WriteFile(replacement, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		return "fixture", nil
	}
	q.mu.Unlock()
	q.get(path)
	q.workers.Wait()
	q.mu.Lock()
	result := q.result
	q.mu.Unlock()
	if result.Qualified || result.State != "unqualified" {
		t.Fatal("binary replaced during probes retained qualification")
	}
}

func TestRestrictedLocalVersionTimeoutKillsDescendantPipeHolder(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n/bin/sh -c '/bin/sleep 2' &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := measureRestrictedLocalCodex(ctx, binary, t.TempDir(), nil); err == nil {
		t.Fatal("hung version probe qualified")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("version descendant kept probe alive past deadline: %v", elapsed)
	}
}

func awaitQualificationResult(t *testing.T, q *localQualificationCache, path string) RestrictedLocalQualification {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		result := q.get(path)
		if result.State != "pending" {
			return result
		}
		if time.Now().After(deadline) {
			t.Fatal("qualification did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitGlobalQualification(t *testing.T) {
	t.Helper()
	t.Cleanup(StopRestrictedLocalQualification)
	deadline := time.Now().Add(5 * time.Second)
	for RestrictedLocalQualificationStatus().State == "pending" {
		if time.Now().After(deadline) {
			t.Fatal("fixture qualification did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}
