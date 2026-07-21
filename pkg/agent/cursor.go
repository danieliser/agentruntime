package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

// CursorAgent builds commands for the cursor-agent CLI.
//
// Prompt mode only — the cursor TUI has no pipe-friendly interactive
// protocol. Effort has no dedicated flag: it is encoded in the model
// string, e.g. "claude-opus-4-8[effort=high]".
//
// KNOWN CONTAMINATION: cursor account-level user rules sync server-side
// into every session and cannot be stripped locally. Sessions report
// "cursor-account-rules" in contamination metadata.
type CursorAgent struct{}

func (a *CursorAgent) Name() string { return "cursor" }

func (a *CursorAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
	if cfg.Interactive {
		return nil, fmt.Errorf("cursor does not support interactive mode: prompt mode only")
	}
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	cmd := []string{"cursor-agent", "--print", "--output-format", "stream-json", "--force", "--trust"}
	if cfg.Model != "" {
		cmd = append(cmd, "--model", cfg.Model)
	}
	if cfg.ResumeSessionID != "" {
		cmd = append(cmd, "--resume", cfg.ResumeSessionID)
	}
	cmd = append(cmd, prompt)

	return cmd, nil
}

// ParseOutput scans cursor-agent stream-json output for the result event:
// {"type":"result","subtype":"success","is_error":false,"result":"...",...}
func (a *CursorAgent) ParseOutput(output []byte) (*AgentResult, bool) {
	if len(output) == 0 {
		return nil, false
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal(line, &parsed); err != nil {
			continue
		}
		if parsed["type"] != "result" {
			continue
		}

		result := &AgentResult{
			Metadata: make(map[string]any),
		}
		if summary, ok := parsed["result"].(string); ok {
			result.Summary = summary
		}
		if isError, ok := parsed["is_error"].(bool); ok && isError {
			result.ExitCode = 1
		}
		if subtype, ok := parsed["subtype"].(string); ok {
			result.Metadata["subtype"] = subtype
		}
		if sessionID, ok := parsed["session_id"].(string); ok {
			result.Metadata["session_id"] = sessionID
		}
		if dur, ok := parsed["duration_ms"]; ok {
			result.Metadata["duration_ms"] = dur
		}
		return result, true
	}

	return nil, false
}
