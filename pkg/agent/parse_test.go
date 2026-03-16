package agent

import (
	"encoding/json"
	"testing"
)

// --- Claude ParseOutput TDD ---

// Claude Code emits NDJSON with stream-json output format. The structured result
// appears as a JSON object with type: "result" containing summary and metadata.

func TestClaudeParseOutput_ResultSummaryBlock(t *testing.T) {
	// Claude Code's --output-format stream-json emits NDJSON lines.
	// A "result" line looks like:
	// {"type":"result","subtype":"success","cost_usd":0.05,"duration_ms":3200,"result":"Fixed the bug in auth.go"}
	output := []byte(`{"type":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll fix the bug."}]}}
{"type":"result","subtype":"success","cost_usd":0.05,"duration_ms":3200,"result":"Fixed the bug in auth.go"}
`)
	a := &ClaudeAgent{}
	result, ok := a.ParseOutput(output)
	if !ok {
		t.Fatal("expected ParseOutput to return ok=true for output with result line")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "Fixed the bug in auth.go" {
		t.Fatalf("expected summary 'Fixed the bug in auth.go', got %q", result.Summary)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected ExitCode 0 for success, got %d", result.ExitCode)
	}
}

func TestClaudeParseOutput_ErrorResult(t *testing.T) {
	output := []byte(`{"type":"result","subtype":"error_max_turns","cost_usd":0.10,"result":"Ran out of turns"}
`)
	a := &ClaudeAgent{}
	result, ok := a.ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for error result")
	}
	if result.ExitCode == 0 {
		t.Fatal("expected non-zero exit code for error result")
	}
	if result.Summary != "Ran out of turns" {
		t.Fatalf("expected summary about turns, got %q", result.Summary)
	}
}

func TestClaudeParseOutput_NoResultLine(t *testing.T) {
	output := []byte(`{"type":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working..."}]}}
`)
	a := &ClaudeAgent{}
	result, ok := a.ParseOutput(output)
	if ok {
		t.Fatal("expected ok=false when no result line present")
	}
	if result != nil {
		t.Fatal("expected nil result")
	}
}

func TestClaudeParseOutput_EmptyInput(t *testing.T) {
	a := &ClaudeAgent{}
	result, ok := a.ParseOutput([]byte{})
	if ok {
		t.Fatal("expected ok=false for empty input")
	}
	if result != nil {
		t.Fatal("expected nil result")
	}
}

func TestClaudeParseOutput_CostInMetadata(t *testing.T) {
	output := []byte(`{"type":"result","subtype":"success","cost_usd":0.12,"duration_ms":5000,"result":"Done"}
`)
	a := &ClaudeAgent{}
	result, ok := a.ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true")
	}
	cost, exists := result.Metadata["cost_usd"]
	if !exists {
		t.Fatal("expected cost_usd in metadata")
	}
	// JSON numbers unmarshal as float64.
	if cost.(float64) != 0.12 {
		t.Fatalf("expected cost 0.12, got %v", cost)
	}
}

// --- Codex ParseOutput TDD ---

// Codex CLI emits NDJSON events. The completion is signaled by a line
// with type: "message.completed" or type: "response.completed" containing
// the final output.

func TestCodexParseOutput_MessageCompleted(t *testing.T) {
	// Codex --quiet output is NDJSON. The final event has the result.
	output := []byte(`{"type":"message.delta","content":"working..."}
{"type":"message.completed","content":"Implemented the feature in main.go"}
`)
	a := &CodexAgent{}
	result, ok := a.ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for output with message.completed")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary == "" {
		t.Fatal("expected non-empty summary")
	}
}

func TestCodexParseOutput_ResponseCompleted(t *testing.T) {
	// Alternative event format.
	output := []byte(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"Fixed the tests"}]}]}}
`)
	a := &CodexAgent{}
	result, ok := a.ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for response.completed")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestCodexParseOutput_NoCompletion(t *testing.T) {
	output := []byte(`{"type":"message.delta","content":"still working"}
`)
	a := &CodexAgent{}
	result, ok := a.ParseOutput(output)
	if ok {
		t.Fatal("expected ok=false when no completion event")
	}
	if result != nil {
		t.Fatal("expected nil result")
	}
}

func TestCodexParseOutput_EmptyInput(t *testing.T) {
	a := &CodexAgent{}
	result, ok := a.ParseOutput([]byte{})
	if ok {
		t.Fatal("expected ok=false for empty input")
	}
	if result != nil {
		t.Fatal("expected nil result")
	}
}

// --- Verify JSON structure helper ---

func TestClaudeResultLine_JSONStructure(t *testing.T) {
	// Verify that the result line we parse matches expected JSON structure.
	line := `{"type":"result","subtype":"success","cost_usd":0.05,"duration_ms":3200,"result":"summary text"}`
	var parsed map[string]any
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["type"] != "result" {
		t.Fatalf("expected type 'result', got %v", parsed["type"])
	}
}
