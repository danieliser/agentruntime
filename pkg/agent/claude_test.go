package agent

import (
	"slices"
	"testing"
)

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

func TestClaudeBuildCmdEnforcesRestrictedToolAndPermissionPolicy(t *testing.T) {
	cmd, err := (&ClaudeAgent{}).BuildCmd("", AgentConfig{
		NativeStream: true, EnforcePolicy: true, PermissionMode: "dontAsk",
		AllowedTools: []string{"WebSearch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd, "--dangerously-skip-permissions") || slices.Contains(cmd, "--allow-dangerously-skip-permissions") {
		t.Fatalf("restricted Claude command bypasses permissions: %v", cmd)
	}
	for _, sequence := range [][2]string{
		{"--permission-mode", "dontAsk"}, {"--tools", "WebSearch"},
		{"--mcp-config", `{"mcpServers":{}}`},
	} {
		if !containsSequence(cmd, sequence[0], sequence[1]) {
			t.Fatalf("restricted Claude command missing %v: %v", sequence, cmd)
		}
	}
	for _, flag := range []string{"--strict-mcp-config", "--disable-slash-commands", "--safe-mode"} {
		if !slices.Contains(cmd, flag) {
			t.Fatalf("restricted Claude command missing %s: %v", flag, cmd)
		}
	}
}

func TestClaudeBuildCmdAppliesNativeJSONSchema(t *testing.T) {
	const schema = `{"type":"object","required":["url"]}`
	cmd, err := (&ClaudeAgent{}).BuildCmd("", AgentConfig{NativeStream: true, JSONSchema: []byte(schema)})
	if err != nil {
		t.Fatal(err)
	}
	if !containsSequence(cmd, "--json-schema", schema) {
		t.Fatalf("Claude command missing exact JSON Schema: %v", cmd)
	}
}
