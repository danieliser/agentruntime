package agent

import (
	"testing"
)

// Fuzz targets use Go's native fuzzing (go test -fuzz). They generate random
// inputs to find panics, crashes, and unexpected behavior in ParseOutput.
// Run: go test -fuzz=FuzzClaudeParseOutput -fuzztime=30s ./pkg/agent/

// FuzzClaudeParseOutput feeds random bytes to ClaudeAgent.ParseOutput.
// Must never panic regardless of input.
func FuzzClaudeParseOutput(f *testing.F) {
	// Seed corpus — known-good and known-tricky inputs.
	f.Add([]byte(`{"type":"result","subtype":"success","result":"done"}`))
	f.Add([]byte(`{"type":"result","subtype":"error_max_turns","result":"failed"}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(``))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":"result"}`))
	f.Add([]byte(`{"type":"result","result":null}`))
	f.Add([]byte(`{"type":"result","result":42}`))
	f.Add([]byte("{\"type\":\"result\",\"result\":\"has\\x00null\"}"))
	f.Add([]byte(`{"type":"init"}` + "\n" + `{"type":"result","subtype":"success","result":"x"}`))

	a := &ClaudeAgent{}
	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic — that's the only assertion for fuzz.
		result, ok := a.ParseOutput(data)
		if ok && result == nil {
			t.Error("ok=true but result is nil — contract violation")
		}
		if !ok && result != nil {
			t.Error("ok=false but result is non-nil — contract violation")
		}
	})
}

// FuzzCodexParseOutput feeds random bytes to CodexAgent.ParseOutput.
func FuzzCodexParseOutput(f *testing.F) {
	f.Add([]byte(`{"type":"message.completed","content":"done"}`))
	f.Add([]byte(`{"type":"response.completed","response":{"output":[]}}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":"message.completed"}`))
	f.Add([]byte(`{"type":"message.completed","content":123}`))

	a := &CodexAgent{}
	f.Fuzz(func(t *testing.T, data []byte) {
		result, ok := a.ParseOutput(data)
		if ok && result == nil {
			t.Error("ok=true but result is nil — contract violation")
		}
		if !ok && result != nil {
			t.Error("ok=false but result is non-nil — contract violation")
		}
	})
}

// FuzzClaudeBuildCmd feeds random prompts and models to BuildCmd.
// Must never panic. Must return error for empty prompt.
func FuzzClaudeBuildCmd(f *testing.F) {
	f.Add("normal prompt", "claude-sonnet-4-6")
	f.Add("", "")
	f.Add("$(rm -rf /)", "")
	f.Add("prompt\x00with\x00nulls", "model\nwith\nnewlines")
	f.Add("\n\n\n", "")

	a := &ClaudeAgent{}
	f.Fuzz(func(t *testing.T, prompt, model string) {
		cmd, err := a.BuildCmd(prompt, AgentConfig{Model: model})
		if prompt == "" {
			if err == nil {
				t.Error("expected error for empty prompt")
			}
			return
		}
		if err != nil {
			return // non-empty prompt can still fail for other reasons
		}
		if len(cmd) < 2 {
			t.Errorf("cmd too short: %v", cmd)
		}
		// Verify prompt appears exactly once and verbatim (no shell splitting).
		found := 0
		for _, arg := range cmd {
			if arg == prompt {
				found++
			}
		}
		if found != 1 {
			t.Errorf("prompt should appear exactly once in cmd, found %d times", found)
		}
	})
}
