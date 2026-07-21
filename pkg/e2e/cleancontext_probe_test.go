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

// hardLeakTells are strings that must NEVER appear in a clean-context probe
// answer — each one is a specific host-config artifact from this machine
// class of leak (MCP servers, memory tooling, personas, house tooling).
var hardLeakTells = []string{
	"wp-mcp-router",
	"tessera",
	"cloudways",
	"AutoMem",
	"automem",
	"Apex",
	"PERSONALITY",
	"wp-test",
	"terse-build",
}

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
	// still must not show MCP servers or host instruction files.
	answer := runCleanContextProbe(t, cursorPath, `{"context":"clean"}`)
	assertProbeClean(t, "cursor", answer, []string{"user rule", "rules"})
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

// assertProbeClean fails if the probe answer contains any hard leak tell.
// acceptedResiduals documents known-unstrippable items for the route; they
// are allowed in the answer but logged for visibility.
func assertProbeClean(t *testing.T, route, answer string, acceptedResiduals []string) {
	t.Helper()

	lower := strings.ToLower(answer)
	for _, tell := range hardLeakTells {
		if strings.Contains(lower, strings.ToLower(tell)) {
			t.Errorf("[%s] CONTAMINATED: probe answer mentions %q\nanswer: %s", route, tell, answer)
		}
	}
	if t.Failed() {
		return
	}

	if strings.Contains(answer, "NONE") {
		t.Logf("[%s] probe clean: NONE", route)
		return
	}
	for _, residual := range acceptedResiduals {
		if strings.Contains(lower, strings.ToLower(residual)) {
			t.Logf("[%s] probe clean with accepted residual %q:\n%s", route, residual, answer)
			return
		}
	}
	// No NONE and no recognized residual — surface the answer for a human
	// call rather than silently passing.
	t.Errorf("[%s] probe answer neither NONE nor a recognized residual — review:\n%s", route, answer)
}
