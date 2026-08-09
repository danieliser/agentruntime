package agent

import "testing"

func TestClaudeBuildCmd_WithResumeSession(t *testing.T) {
	a := &ClaudeAgent{}

	cmd, err := a.BuildCmd("continue", AgentConfig{ResumeSessionID: "claude-session-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !containsSequence(cmd, "--resume", "claude-session-123") {
		t.Fatalf("expected --resume claude-session-123 in cmd, got %v", cmd)
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

func TestClaudeBuildCmd_Interactive(t *testing.T) {
	a := &ClaudeAgent{}

	cmd, err := a.BuildCmd("", AgentConfig{Interactive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if contains(cmd, "-p") {
		t.Fatalf("did not expect -p in interactive cmd, got %v", cmd)
	}
	if !containsSequence(cmd, "--output-format", "stream-json") {
		t.Fatalf("expected --output-format stream-json in cmd, got %v", cmd)
	}
}

func TestClaudeBuildCmd_NativeStreamUsesBidirectionalJSON(t *testing.T) {
	a := &ClaudeAgent{}
	cmd, err := a.BuildCmd("", AgentConfig{NativeStream: true, SessionID: "logical-session"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, sequence := range [][2]string{
		{"--input-format", "stream-json"},
		{"--output-format", "stream-json"},
		{"--session-id", "logical-session"},
	} {
		if !containsSequence(cmd, sequence[0], sequence[1]) {
			t.Fatalf("expected %v in native command, got %v", sequence, cmd)
		}
	}
	for _, flag := range []string{"--include-partial-messages", "--ide"} {
		if !contains(cmd, flag) {
			t.Fatalf("expected %s in native command, got %v", flag, cmd)
		}
	}
	if contains(cmd, "-p") {
		t.Fatalf("native stream prompt must arrive over stdin, got %v", cmd)
	}
}
