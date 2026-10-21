package runtime

import (
	"context"
	"github.com/danieliser/agentruntime/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestrictedLocalClaudeVersionAloneDoesNotQualify(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '2.1.305 (Claude Code)\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := probeRestrictedLocalClaude(context.Background(), binary, t.TempDir()); err == nil {
		t.Fatal("version alone qualified")
	}
}

func TestRestrictedLocalClaudeNoModelQualification(t *testing.T) {
	if os.Getenv("AGENTD_RESTRICTED_LOCAL_PROBE") != "1" {
		t.Skip("opt-in no-model qualification")
	}
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 415*time.Second)
	defer cancel()
	version, err := measureRestrictedLocalClaude(ctx, binary, t.TempDir(), func(name string) { t.Log("PASS", name) })
	if err != nil {
		t.Fatal(err)
	}
	t.Log("measured", version, "mode=file-tools-only; Bash confinement unproven and Bash unavailable")
}

func TestRestrictedLocalClaudeFixtureQualification(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(binary, []byte(testutil.ClaudeQualificationScript("exit 1")), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := measureRestrictedLocalClaude(ctx, binary, t.TempDir(), func(name string) { t.Log(name) })
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestrictedLocalClaudeRejectsWeakenedReadbacks(t *testing.T) {
	for _, mutation := range []string{"settings", "settings-errors", "rules", "bypass", "malformed"} {
		t.Run(mutation, func(t *testing.T) {
			script := testutil.ClaudeQualificationScript("exit 1")
			switch mutation {
			case "settings":
				script = strings.Replace(script, `/usr/bin/tr -d '\n' < "$settings"`, `printf '{}'`, 1)
			case "settings-errors":
				script = strings.Replace(script, `"response":{"effective":`, `"response":{"errors":[{"message":"synthetic validation failure"}],"effective":`, 1)
			case "rules":
				script = strings.Replace(script, `{"behavior":"deny","rule":"Bash"}`, `{"behavior":"allow","rule":"Bash"}`, 1)
			case "bypass":
				script = strings.Replace(script, `"error_code":"bypass_restricted"`, `"error_code":"different_error"`, 1)
			case "malformed":
				script = strings.Replace(script, `"claude_code_version":"2.1.fixture"`, `"claude_code_version":false`, 1)
			}
			binary := filepath.Join(t.TempDir(), "claude")
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := measureRestrictedLocalClaude(ctx, binary, t.TempDir(), nil); err == nil {
				t.Fatal("weakened provider qualified")
			}
		})
	}
}

func TestRestrictedLocalCodexQualificationErrorCompatibility(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := CheckRestrictedLocalProviderPermissions("codex", "")
	if err == nil || err.Error() != "restricted_local: execution_policy_unsupported: provider has no qualified read-confined local sandbox" {
		t.Fatal("v2.4.0 spawn error text changed", err)
	}
}

func TestClaudeQualificationCacheReportsModeAndInvalidates(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	q := localQualificationCache{agent: "claude", platform: "darwin", root: t.TempDir(), probe: func(context.Context, string, string) (string, error) { calls.Add(1); return "fixture", nil }}
	t.Cleanup(q.stop)
	result := awaitQualificationResult(t, &q, binary)
	if !result.Qualified || result.Agent != "claude" || result.Mode != "file-tools-only" {
		t.Fatal(result)
	}
	if q.get(binary) != result || calls.Load() != 1 {
		t.Fatal("cache missed unchanged binary")
	}
	stamp := time.Now().Add(time.Minute)
	if err := os.Chtimes(binary, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if next := q.get(binary); next.Qualified || next.State != "pending" || next.Agent != "claude" {
		t.Fatal("changed binary retained authority", next)
	}
	if !awaitQualificationResult(t, &q, binary).Qualified || calls.Load() != 2 {
		t.Fatal("changed binary not requalified")
	}
}
