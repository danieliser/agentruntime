//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Clean-context isolation self-tests.
//
// Pattern from GameDevTests harness/verify-isolation.sh --probe: launch the
// CLI through the sidecar with context:"clean" against the REAL host HOME
// and ask the model to enumerate its own context. Probing the model is the
// only reliable check — config inspection lies (probes, not greps, caught
// the SessionStart-hook and CLAUDE.md leaks in the 2026-07-20 audit).
//
// These tests cost one small LLM call each and require the CLI + auth on
// the machine; they skip otherwise.

const probeQuestion = "In one short list: name any MCP servers, user rules, skills, personas, or global instruction files (CLAUDE.md, AGENTS.md, GROK.md) you were given beyond your base system prompt. If none, say NONE. Create no files."

func TestE2E_CleanContext_ClaudeProbe(t *testing.T) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not in PATH")
	}

	// Residual (accepted): the CLI's default bundled skills ship with the
	// binary and remain under --safe-mode.
	answer := runCleanContextProbe(t, claudePath, `{"context":"clean"}`)
	assertProbeClean(t, "claude", answer, []string{"bundled", "built-in", "builtin", "default"})
}

func TestE2E_CleanContext_CodexProbe(t *testing.T) {
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not in PATH")
	}

	// Residual (accepted): codex 0.144 built-in skills ship with the binary.
	answer := runCleanContextProbe(t, codexPath, `{"context":"clean"}`)
	assertProbeClean(t, "codex", answer, []string{"skill-creator", "skill-installer", "imagegen", "openai-docs", "plugin-creator"})
}

func TestE2E_CleanContext_GrokProbe(t *testing.T) {
	grokPath, err := exec.LookPath("grok")
	if err != nil {
		t.Skip("grok not in PATH")
	}

	// Residual (accepted): 4 built-in CLI skills ship with the binary.
	answer := runCleanContextProbe(t, grokPath, `{"context":"clean"}`)
	assertProbeClean(t, "grok", answer, []string{"check-work", "create-skill", "imagine", "help"})
}

func TestE2E_CleanContext_CursorProbe(t *testing.T) {
	cursorPath, err := exec.LookPath("cursor-agent")
	if err != nil {
		t.Skip("cursor-agent not in PATH")
	}

	// Residual (accepted, server-side): account-level user rules. The probe
	// still must not show MCP servers or host instruction files. "account"
	// instead of a bare "rules" so host rule files can't slip through.
	answer := runCleanContextProbe(t, cursorPath, `{"context":"clean"}`)
	assertProbeClean(t, "cursor", answer, []string{"user rule", "account"})
}

// runCleanContextProbe starts the sidecar with the REAL host HOME (isolation
// must come from the clean-context materialization, not the test harness),
// sends the probe question as the prompt, and returns the concatenated
// non-thought agent text.
func runCleanContextProbe(t *testing.T, agentPath, agentConfig string) string {
	t.Helper()

	port := freePort(t)
	binary := buildSidecarBinary(t)

	cmd := exec.Command(binary)
	cmd.Dir = t.TempDir() // fresh workspace; HOME stays the real one

	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	cmd.Env = append(os.Environ(),
		"AGENT_CMD="+commandSpec(t, agentPath),
		fmt.Sprintf("SIDECAR_PORT=%d", port),
		"AGENT_PROMPT="+probeQuestion,
		"AGENT_CONFIG="+agentConfig,
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
		}
		if t.Failed() && logs.Len() > 0 {
			t.Logf("sidecar logs:\n%s", logs.String())
		}
	})

	waitForSidecarHealth(t, port)

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	events := readEvents(t, conn, 120*time.Second)

	var answer strings.Builder
	for _, event := range events {
		if eventType(event) != "agent_message" {
			continue
		}
		data, _ := event["data"].(map[string]any)
		if data == nil {
			continue
		}
		if thought, _ := data["thought"].(bool); thought {
			continue
		}
		if text, ok := data["text"].(string); ok {
			answer.WriteString(text)
		}
	}
	if answer.Len() == 0 {
		t.Fatalf("probe produced no agent text; events: %#v", events)
	}
	return answer.String()
}

// assertProbeClean fails on any violation reported by evaluateProbeAnswer
// (see probeassert.go, unit-tested outside the e2e tag). Structure-aware:
// the answer must be NONE or a list whose every item matches an accepted
// residual — a broad keyword anywhere in prose is not a pass.
func assertProbeClean(t *testing.T, route, answer string, acceptedResiduals []string) {
	t.Helper()

	violations := evaluateProbeAnswer(answer, hardLeakTells, acceptedResiduals)
	for _, violation := range violations {
		t.Errorf("[%s] CONTAMINATED: %s\nanswer: %s", route, violation, answer)
	}
	if len(violations) == 0 {
		t.Logf("[%s] probe clean:\n%s", route, answer)
	}
}
