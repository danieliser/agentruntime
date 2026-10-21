package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// RestrictedLocalQualification is measured no-model evidence, not a version
// allowlist. Pending qualification never authorizes a restricted local worker.
type RestrictedLocalQualification struct {
	Mode      string    `json:"mode,omitempty"`
	Agent     string    `json:"agent"`
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checked_at"`
	Qualified bool      `json:"qualified"`
	State     string    `json:"state"`
}

type localQualificationCache struct {
	agent      string
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

var restrictedLocalClaudeQualification = localQualificationCache{agent: "claude", platform: goruntime.GOOS, probe: probeRestrictedLocalClaude}

var restrictedLocalQualification = localQualificationCache{platform: goruntime.GOOS, probe: probeRestrictedLocalCodex}

// InitializeRestrictedLocalQualification schedules measurement after AgentD has
// opened its listener. Neither initialization nor a cache lookup waits for probes.
func InitializeRestrictedLocalQualification(dataDir string) {
	for _, q := range []*localQualificationCache{&restrictedLocalQualification, &restrictedLocalClaudeQualification} {
		q.mu.Lock()
		q.invalidateLocked()
		q.root = dataDir
		q.closed = false
		q.mu.Unlock()
	}
	RestrictedLocalQualificationsStatus()
}

// StopRestrictedLocalQualification cancels and joins the supervised workers.
// Server.Start owns this lifetime; admissions can never start jobs after stop.
func StopRestrictedLocalQualification() {
	restrictedLocalQualification.stop()
	restrictedLocalClaudeQualification.stop()
}

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
	q.result = RestrictedLocalQualification{Agent: q.agentName(), Mode: q.mode(), State: "unqualified"}
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

// RestrictedLocalQualificationForAgent never authorizes pending or unknown providers.
func RestrictedLocalQualificationForAgent(agent string) RestrictedLocalQualification {
	q := &restrictedLocalQualification
	if agent == "claude" {
		q = &restrictedLocalClaudeQualification
	} else if agent != "codex" {
		return RestrictedLocalQualification{Agent: agent, State: "unqualified"}
	}
	path, err := exec.LookPath(agent)
	if err != nil {
		path = ""
	}
	return q.get(path)
}

// Codex's original top-level fields remain available; agents carries each result.
type RestrictedLocalQualifications struct {
	RestrictedLocalQualification
	Agents map[string]RestrictedLocalQualification `json:"agents"`
}

func RestrictedLocalQualificationsStatus() RestrictedLocalQualifications {
	codex := RestrictedLocalQualificationStatus()
	return RestrictedLocalQualifications{RestrictedLocalQualification: codex, Agents: map[string]RestrictedLocalQualification{"codex": codex, "claude": RestrictedLocalQualificationForAgent("claude")}}
}
func (q *localQualificationCache) agentName() string {
	if q.agent == "" {
		return "codex"
	}
	return q.agent
}
func (q *localQualificationCache) mode() string {
	if q.agentName() == "claude" {
		return "file-tools-only"
	}
	return "bash"
}

func sameQualificationBinary(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.ModTime().Equal(after.ModTime()) && before.Size() == after.Size() && before.Mode() == after.Mode()
}

func (q *localQualificationCache) get(path string) RestrictedLocalQualification {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.platform != "darwin" || q.root == "" {
		return RestrictedLocalQualification{Agent: q.agentName(), Mode: q.mode(), State: "unqualified"}
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
	q.result = RestrictedLocalQualification{Agent: q.agentName(), Mode: q.mode(), State: "pending"}
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
		result := RestrictedLocalQualification{Agent: q.agentName(), Mode: q.mode(), Version: version, CheckedAt: time.Now().UTC(), State: "unqualified"}
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
	return qualifiedRestrictedLocalAgents("bash")
}

// File-only workers are separate from coding workers that can execute a shell.
func QualifiedRestrictedLocalFileToolsAgents() []string {
	return qualifiedRestrictedLocalAgents("file-tools-only")
}

func qualifiedRestrictedLocalAgents(mode string) []string {
	agents := []string{}
	for _, name := range []string{"codex", "claude"} {
		if q := RestrictedLocalQualificationForAgent(name); q.Qualified && q.Mode == mode {
			agents = append(agents, name)
		}
	}
	return agents
}

// CheckRestrictedLocalProviderPermissions is shared by API admission and spawn.
// File-only Claude must never silently replace a worker with shell access.
func CheckRestrictedLocalProviderPermissions(agentName, tools string) error {
	reject := func(message string) error {
		return durable.NewError(durable.CodeExecutionPolicyUnsupported, "restricted_local", message, nil)
	}
	if agentName == "claude" && tools != "file_only" {
		return reject("restricted local Claude requires provider_permissions.tools=file_only; this worker has no shell")
	}
	if tools != "" && (agentName != "claude" || tools != "file_only") {
		return reject("provider_permissions.tools=file_only is supported only for restricted local Claude")
	}
	q := RestrictedLocalQualificationForAgent(agentName)
	if !q.Qualified || (q.Mode != "bash" && !(q.Mode == "file-tools-only" && tools == "file_only")) {
		if agentName == "codex" {
			return reject("provider has no qualified read-confined local sandbox")
		}
		return reject("provider/version/platform has no qualified restricted local tools")
	}
	return nil
}
