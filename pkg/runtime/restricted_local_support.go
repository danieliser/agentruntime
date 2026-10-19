package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"time"
)

// RestrictedLocalQualification is measured no-model evidence, not a version
// allowlist. Pending qualification never authorizes a restricted local worker.
type RestrictedLocalQualification struct {
	Agent     string    `json:"agent"`
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checked_at"`
	Qualified bool      `json:"qualified"`
	State     string    `json:"state"`
}

type localQualificationCache struct {
	mu         sync.Mutex
	workers    sync.WaitGroup
	root       string
	platform   string
	probe      func(context.Context, string, string) (string, error)
	timeout    time.Duration
	path       string
	lookupPath string
	info       os.FileInfo
	result     RestrictedLocalQualification
	cancel     context.CancelFunc
	revision   uint64
	closed     bool
}

var restrictedLocalQualification = localQualificationCache{platform: goruntime.GOOS, probe: probeRestrictedLocalCodex}

// InitializeRestrictedLocalQualification schedules measurement after AgentD has
// opened its listener. Neither initialization nor a cache lookup waits for probes.
func InitializeRestrictedLocalQualification(dataDir string) {
	q := &restrictedLocalQualification
	q.mu.Lock()
	q.invalidateLocked()
	q.root = dataDir
	q.closed = false
	q.mu.Unlock()
	RestrictedLocalQualificationStatus()
}

// StopRestrictedLocalQualification cancels and joins the supervised workers.
// Server.Start owns this lifetime; admissions can never start jobs after stop.
func StopRestrictedLocalQualification() { restrictedLocalQualification.stop() }

func (q *localQualificationCache) stop() {
	q.mu.Lock()
	q.closed = true
	q.invalidateLocked()
	q.mu.Unlock()
	q.workers.Wait()
}

func (q *localQualificationCache) invalidateLocked() {
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
	q.revision++
	q.info = nil
	q.result = RestrictedLocalQualification{Agent: "codex", State: "unqualified"}
}

// RestrictedLocalQualificationStatus stats the executable without launching a
// foreground subprocess. Changed executables return pending immediately and
// requalify in the background; stale success cannot authorize a changed binary.
func RestrictedLocalQualificationStatus() RestrictedLocalQualification {
	path, err := exec.LookPath("codex")
	if err != nil {
		path = ""
	}
	return restrictedLocalQualification.get(path)
}

func sameQualificationBinary(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.ModTime().Equal(after.ModTime()) && before.Size() == after.Size() && before.Mode() == after.Mode()
}

func (q *localQualificationCache) get(path string) RestrictedLocalQualification {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.platform != "darwin" || q.root == "" {
		return RestrictedLocalQualification{Agent: "codex", State: "unqualified"}
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		q.invalidateLocked()
		return q.result
	}
	info, err := os.Stat(physical)
	if err != nil || !info.Mode().IsRegular() {
		q.invalidateLocked()
		return q.result
	}
	if q.path == physical && q.lookupPath == path && sameQualificationBinary(q.info, info) {
		return q.result
	}
	q.invalidateLocked()
	q.path = physical
	q.lookupPath = path
	q.info = info
	q.result = RestrictedLocalQualification{Agent: "codex", State: "pending"}
	timeout := q.timeout
	if timeout == 0 {
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	q.cancel = cancel
	revision, root, probe := q.revision, q.root, q.probe
	q.workers.Add(1)
	go func() {
		defer q.workers.Done()
		defer cancel()
		version, probeErr := probe(ctx, physical, root)
		after, statErr := os.Stat(physical)
		result := RestrictedLocalQualification{Agent: "codex", Version: version, CheckedAt: time.Now().UTC(), State: "unqualified"}
		result.Qualified = probeErr == nil && ctx.Err() == nil && statErr == nil && sameQualificationBinary(info, after)
		if result.Qualified {
			result.State = "qualified"
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		// A canceled/superseded measurement must never overwrite newer evidence.
		if revision == q.revision && !q.closed {
			q.result = result
			q.cancel = nil
		}
	}()
	return q.result
}

func QualifiedRestrictedLocalAgents() []string {
	if RestrictedLocalQualificationStatus().Qualified {
		return []string{"codex"}
	}
	return []string{}
}
