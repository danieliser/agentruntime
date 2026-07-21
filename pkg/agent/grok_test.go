package agent

import (
	"strings"
	"testing"
)

func TestGrokBuildCmd(t *testing.T) {
	a := &GrokAgent{}
	cmd, err := a.BuildCmd("fix the bug", AgentConfig{Model: "grok-4.5", Effort: "high"})
	if err != nil {
		t.Fatalf("BuildCmd() error = %v", err)
	}
	joined := strings.Join(cmd, " ")
	for _, want := range []string{"grok", "--always-approve", "--output-format streaming-json", "--model grok-4.5", "--reasoning-effort high", "-p fix the bug"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cmd %q missing %q", joined, want)
		}
	}
}

func TestGrokBuildCmd_InteractiveRejected(t *testing.T) {
	a := &GrokAgent{}
	if _, err := a.BuildCmd("", AgentConfig{Interactive: true}); err == nil {
		t.Fatal("expected error for interactive mode")
	}
}

func TestGrokBuildCmd_RequiresPrompt(t *testing.T) {
	a := &GrokAgent{}
	if _, err := a.BuildCmd("", AgentConfig{}); err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestGrokParseOutput(t *testing.T) {
	a := &GrokAgent{}
	output := `{"type":"thought","data":"hmm"}
{"type":"text","data":"Hello "}
{"type":"text","data":"world"}
{"type":"end","stopReason":"EndTurn","sessionId":"sess-1","num_turns":1,"total_cost_usd":0.004}
`
	result, ok := a.ParseOutput([]byte(output))
	if !ok {
		t.Fatal("ParseOutput() ok = false, want true")
	}
	if result.Summary != "Hello world" {
		t.Errorf("Summary = %q, want %q", result.Summary, "Hello world")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Metadata["session_id"] != "sess-1" {
		t.Errorf("session_id = %v", result.Metadata["session_id"])
	}
	if result.Metadata["cost_usd"] != 0.004 {
		t.Errorf("cost_usd = %v", result.Metadata["cost_usd"])
	}
}

func TestGrokParseOutput_NoEnd(t *testing.T) {
	a := &GrokAgent{}
	if _, ok := a.ParseOutput([]byte(`{"type":"text","data":"partial"}`)); ok {
		t.Fatal("expected ok = false without end event")
	}
}

func TestGrokRegistry(t *testing.T) {
	r := DefaultRegistry()
	if r.Get("grok") == nil {
		t.Fatal("grok not in default registry")
	}
}
