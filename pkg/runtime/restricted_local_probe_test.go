package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in, real Codex sandbox + app-server initialize/thread-start only. No model
// turn, prompt, login, real credentials or external network request is sent.
func TestRestrictedLocalCodexSandboxConfinement(t *testing.T) {
	if os.Getenv("AGENTD_RESTRICTED_LOCAL_PROBE") != "1" {
		t.Skip("set AGENTD_RESTRICTED_LOCAL_PROBE=1 for no-model qualification")
	}
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	q := localQualificationCache{platform: goruntime.GOOS, root: t.TempDir(), timeout: 45 * time.Second, probe: func(ctx context.Context, binary, root string) (string, error) {
		calls.Add(1)
		return measureRestrictedLocalCodex(ctx, binary, root, func(name string) { t.Log("PASS", name) })
	}}
	t.Cleanup(q.stop)
	initial := q.get(binary)
	if initial.State != "pending" || initial.Qualified {
		t.Fatal("initial measurement must be pending/unqualified")
	}
	q.workers.Wait()
	result := q.get(binary)
	data, _ := json.Marshal(result)
	t.Log("measured qualification:", string(data))
	if !result.Qualified {
		t.Fatal("installed Codex failed qualification")
	}
	cached := q.get(binary)
	if calls.Load() != 1 || cached != result {
		t.Fatal("unchanged binary was not served from the qualification cache")
	}

}
