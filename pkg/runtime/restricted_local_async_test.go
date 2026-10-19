package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func qualificationState(t *testing.T, result RestrictedLocalQualification) string {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	return body.State
}

func TestRestrictedLocalQualificationPendingDoesNotBlock(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	q := localQualificationCache{platform: "darwin", root: t.TempDir(), probe: func(context.Context, string, string) (string, error) {
		close(started)
		<-release
		return "fixture-v1", nil
	}}
	t.Cleanup(q.stop)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer close(release)
	first := make(chan RestrictedLocalQualification, 1)
	workers.Add(1)
	go func() { defer workers.Done(); first <- q.get(binary) }()
	<-started
	second := make(chan RestrictedLocalQualification, 1)
	workers.Add(1)
	go func() { defer workers.Done(); second <- q.get(binary) }()
	for _, result := range []<-chan RestrictedLocalQualification{first, second} {
		select {
		case value := <-result:
			if value.Qualified || qualificationState(t, value) != "pending" {
				t.Error("inflight measurement did not report pending/unqualified")
			}
		case <-time.After(200 * time.Millisecond):
			t.Error("qualification blocked a cache lookup")
		}
	}
}

func TestRestrictedLocalSupersededMeasurementCannotRestoreAuthority(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(firstRelease) }) }
	defer release()
	var calls atomic.Int32
	q := localQualificationCache{platform: "darwin", root: t.TempDir(), probe: func(ctx context.Context, _, _ string) (string, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-firstRelease
			return "old", nil
		}
		return "new", errors.New("new binary failed qualification")
	}}
	t.Cleanup(q.stop)
	q.get(binary)
	<-firstStarted
	stamp := time.Now().Add(time.Minute)
	if err := os.Chtimes(binary, stamp, stamp); err != nil {
		release()
		t.Fatal(err)
	}
	if result := q.get(binary); result.State != "pending" || result.Qualified {
		release()
		t.Fatal("changed binary did not report pending")
	}
	result := awaitQualificationResult(t, &q, binary)
	release()
	q.workers.Wait()
	if result.Qualified || result.Version != "new" {
		t.Fatal("new measurement was not retained", result)
	}
	if current := q.get(binary); current.Qualified || current.Version != "new" || current.State != "unqualified" {
		t.Fatal("old success restored stale authority", current)
	}
}

func TestRestrictedLocalStopCancelsAndJoinsMeasurement(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	q := localQualificationCache{platform: "darwin", root: t.TempDir(), probe: func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return "fixture", ctx.Err()
	}}
	q.get(binary)
	<-started
	q.stop()
	select {
	case <-finished:
	default:
		t.Fatal("stop did not join the measurement")
	}
	if result := q.get(binary); result.State != "unqualified" || result.Qualified {
		t.Fatal("stopped cache authorized or relaunched a worker")
	}
}
