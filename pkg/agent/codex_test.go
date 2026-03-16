package agent

import "testing"

func TestCodexBuildCmd_WithResumeSession(t *testing.T) {
	a := &CodexAgent{}

	cmd, err := a.BuildCmd("continue", AgentConfig{ResumeSessionID: "codex-session-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !containsSequence(cmd, "--session", "codex-session-123") {
		t.Fatalf("expected --session flag in cmd, got %v", cmd)
	}
}

func TestCodexBuildCmd_Interactive(t *testing.T) {
	a := &CodexAgent{}

	cmd, err := a.BuildCmd("", AgentConfig{Interactive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cmd) == 0 || cmd[0] != "codex" {
		t.Fatalf("expected interactive codex cmd to start with codex, got %v", cmd)
	}
	if contains(cmd, "exec") {
		t.Fatalf("did not expect exec subcommand in interactive cmd, got %v", cmd)
	}
}
