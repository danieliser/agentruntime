package agent

import "testing"

func TestClaudeBuildCmd_WithResumeSession(t *testing.T) {
	a := &ClaudeAgent{}

	cmd, err := a.BuildCmd("continue", AgentConfig{ResumeSessionID: "claude-session-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !contains(cmd, "--resume") {
		t.Fatalf("expected --resume flag in cmd, got %v", cmd)
	}
	if !containsSequence(cmd, "--session-id", "claude-session-123") {
		t.Fatalf("expected --session-id claude-session-123 in cmd, got %v", cmd)
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
