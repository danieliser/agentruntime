package agent

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClaudeParseOutput_MultipleResultLinesReturnsFirst(t *testing.T) {
	output := []byte(`{"type":"result","subtype":"success","result":"first summary"}
{"type":"result","subtype":"error_max_turns","result":"second summary"}
`)

	result, ok := (&ClaudeAgent{}).ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for first result line")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "first summary" {
		t.Fatalf("expected first result summary, got %q", result.Summary)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0 from first success result, got %d", result.ExitCode)
	}
}

func TestClaudeParseOutput_MissingResultFieldStillReturnsOK(t *testing.T) {
	output := []byte(`{"type":"result","subtype":"success","duration_ms":42}
`)

	result, ok := (&ClaudeAgent{}).ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for result line without result field")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "" {
		t.Fatalf("expected empty summary when result field is missing, got %q", result.Summary)
	}
	if got := result.Metadata["subtype"]; got != "success" {
		t.Fatalf("expected subtype metadata to survive, got %v", got)
	}
}

func TestClaudeParseOutput_NonStringResultHandledGracefully(t *testing.T) {
	output := []byte(`{"type":"result","subtype":"success","result":12345}
`)

	result, ok := (&ClaudeAgent{}).ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for result line with numeric result")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "" {
		t.Fatalf("expected empty summary for non-string result, got %q", result.Summary)
	}
}

func TestClaudeParseOutput_ResultAfterOneHundredThousandLines(t *testing.T) {
	var output bytes.Buffer
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&output, "{\"type\":\"delta\",\"index\":%d}\n", i)
	}
	output.WriteString(`{"type":"result","subtype":"success","result":"needle"}` + "\n")

	start := time.Now()
	result, ok := (&ClaudeAgent{}).ParseOutput(output.Bytes())
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("expected ok=true for buried result line")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "needle" {
		t.Fatalf("expected buried result summary, got %q", result.Summary)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("expected parse to complete within 5s, took %v", elapsed)
	}
}

func TestClaudeParseOutput_MalformedJSONUntilFinalValidResult(t *testing.T) {
	var output bytes.Buffer
	for i := 0; i < 512; i++ {
		output.WriteString("{not-json}\n")
	}
	output.WriteString(`{"type":"result","subtype":"success","result":"eventual success"}` + "\n")

	result, ok := (&ClaudeAgent{}).ParseOutput(output.Bytes())
	if !ok {
		t.Fatal("expected ok=true when last line is a valid result")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "eventual success" {
		t.Fatalf("expected final valid result summary, got %q", result.Summary)
	}
}

func TestClaudeParseOutput_NullBytesInResultText(t *testing.T) {
	output := []byte("{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"hello\\u0000world\"}\n")

	result, ok := (&ClaudeAgent{}).ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for escaped null byte in JSON string")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "hello\x00world" {
		t.Fatalf("expected null byte to survive decoding, got %q", result.Summary)
	}
}

func TestClaudeParseOutput_OneMassiveLineWithoutNewlines(t *testing.T) {
	output := bytes.Repeat([]byte("x"), 10*1024*1024)

	result, ok := (&ClaudeAgent{}).ParseOutput(output)
	if ok {
		t.Fatal("expected ok=false for a massive single token with no result line")
	}
	if result != nil {
		t.Fatal("expected nil result for unparseable massive line")
	}
}

func TestCodexParseOutput_ResponseCompletedDeeplyNestedEmptyArrays(t *testing.T) {
	output := []byte(`{"type":"response.completed","response":{"output":[[],[[]],{"content":[[],[[]],{}]}]}}
`)

	result, ok := (&CodexAgent{}).ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for response.completed even with unusable nested arrays")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "" {
		t.Fatalf("expected empty summary when no text is discoverable, got %q", result.Summary)
	}
}

func TestCodexParseOutput_MessageCompletedWithNumericContent(t *testing.T) {
	output := []byte(`{"type":"message.completed","content":12345}
`)

	result, ok := (&CodexAgent{}).ParseOutput(output)
	if !ok {
		t.Fatal("expected ok=true for message.completed with numeric content")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary != "" {
		t.Fatalf("expected empty summary for non-string content, got %q", result.Summary)
	}
}

func TestParseOutput_WhitespaceOnlyLines(t *testing.T) {
	tests := []struct {
		name  string
		agent Agent
	}{
		{name: "claude", agent: &ClaudeAgent{}},
		{name: "codex", agent: &CodexAgent{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := tt.agent.ParseOutput([]byte(" \n\t\n\r\n"))
			if ok {
				t.Fatal("expected ok=false for whitespace-only output")
			}
			if result != nil {
				t.Fatal("expected nil result for whitespace-only output")
			}
		})
	}
}

func TestParseOutput_CRLFLineEndings(t *testing.T) {
	claudeOutput := []byte("{\"type\":\"init\"}\r\n{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"done\"}\r\n")
	codexOutput := []byte("{\"type\":\"message.delta\",\"content\":\"working\"}\r\n{\"type\":\"message.completed\",\"content\":\"done\"}\r\n")

	claudeResult, ok := (&ClaudeAgent{}).ParseOutput(claudeOutput)
	if !ok || claudeResult == nil {
		t.Fatal("expected Claude parser to accept CRLF output")
	}
	if claudeResult.Summary != "done" {
		t.Fatalf("expected Claude CRLF summary %q, got %q", "done", claudeResult.Summary)
	}

	codexResult, ok := (&CodexAgent{}).ParseOutput(codexOutput)
	if !ok || codexResult == nil {
		t.Fatal("expected Codex parser to accept CRLF output")
	}
	if codexResult.Summary != "done" {
		t.Fatalf("expected Codex CRLF summary %q, got %q", "done", codexResult.Summary)
	}
}

func TestParseOutput_IgnoresTrailingDataAfterCompletion(t *testing.T) {
	claudeOutput := []byte(strings.Join([]string{
		`{"type":"result","subtype":"success","result":"claude done"}`,
		`this is trailing garbage that should never be read`,
	}, "\n"))
	codexOutput := []byte(strings.Join([]string{
		`{"type":"message.completed","content":"codex done"}`,
		`{"type":"message.completed","content":"late overwrite"}`,
	}, "\n"))

	claudeResult, ok := (&ClaudeAgent{}).ParseOutput(claudeOutput)
	if !ok || claudeResult == nil {
		t.Fatal("expected Claude completion before trailing data")
	}
	if claudeResult.Summary != "claude done" {
		t.Fatalf("expected Claude to stop at first completion, got %q", claudeResult.Summary)
	}

	codexResult, ok := (&CodexAgent{}).ParseOutput(codexOutput)
	if !ok || codexResult == nil {
		t.Fatal("expected Codex completion before trailing data")
	}
	if codexResult.Summary != "codex done" {
		t.Fatalf("expected Codex to stop at first completion, got %q", codexResult.Summary)
	}
}
