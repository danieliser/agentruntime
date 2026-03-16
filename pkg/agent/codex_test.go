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
