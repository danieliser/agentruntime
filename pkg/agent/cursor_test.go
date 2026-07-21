package agent

import (
	"strings"
	"testing"
)

func TestCursorBuildCmd(t *testing.T) {
	a := &CursorAgent{}
	cmd, err := a.BuildCmd("fix the bug", AgentConfig{Model: "grok-4.5"})
	if err != nil {
		t.Fatalf("BuildCmd() error = %v", err)
	}
	joined := strings.Join(cmd, " ")
	for _, want := range []string{"cursor-agent", "--print", "--output-format stream-json", "--force", "--trust", "--model grok-4.5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cmd %q missing %q", joined, want)
		}
	}
	if cmd[len(cmd)-1] != "fix the bug" {
		t.Errorf("prompt must be last arg, cmd = %v", cmd)
	}
}

func TestCursorBuildCmd_InteractiveRejected(t *testing.T) {
	a := &CursorAgent{}
	if _, err := a.BuildCmd("", AgentConfig{Interactive: true}); err == nil {
		t.Fatal("expected error for interactive mode")
	}
}

func TestCursorParseOutput(t *testing.T) {
	a := &CursorAgent{}
	output := `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"DONE"}]}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":6641,"result":"DONE","session_id":"s1"}
`
	result, ok := a.ParseOutput([]byte(output))
	if !ok {
		t.Fatal("ParseOutput() ok = false, want true")
	}
	if result.Summary != "DONE" {
		t.Errorf("Summary = %q, want DONE", result.Summary)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Metadata["session_id"] != "s1" {
		t.Errorf("session_id = %v", result.Metadata["session_id"])
	}
}

func TestCursorParseOutput_Error(t *testing.T) {
	a := &CursorAgent{}
	output := `{"type":"result","subtype":"error","is_error":true,"result":"failed"}`
	result, ok := a.ParseOutput([]byte(output))
	if !ok {
		t.Fatal("ParseOutput() ok = false, want true")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
}

func TestCursorRegistry(t *testing.T) {
	r := DefaultRegistry()
	if r.Get("cursor") == nil {
		t.Fatal("cursor not in default registry")
	}
}
