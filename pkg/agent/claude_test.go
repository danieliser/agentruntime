package agent

import "testing"

func TestClaudeBuildCmd_WithResumeSession(t *testing.T) {
	a := &ClaudeAgent{}

	cmd, err := a.BuildCmd("continue", AgentConfig{ResumeSessionID: "claude-session-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !containsSequence(cmd, "--resume", "--session-id", "claude-session-123") {
		t.Fatalf("expected resume flags in cmd, got %v", cmd)
	}
}

func TestClaudeBuildCmd_WithoutResumeSession(t *testing.T) {
	a := &ClaudeAgent{}

	cmd, err := a.BuildCmd("continue", AgentConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if contains(cmd, "--resume") {
		t.Fatalf("did not expect --resume in cmd, got %v", cmd)
	}
	if contains(cmd, "--session-id") {
		t.Fatalf("did not expect --session-id in cmd, got %v", cmd)
	}
}
