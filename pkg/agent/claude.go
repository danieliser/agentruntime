package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

// ClaudeAgent builds commands for Claude Code CLI.
type ClaudeAgent struct{}

func (a *ClaudeAgent) Name() string { return "claude" }

func (a *ClaudeAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
	if !cfg.Interactive && prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	cmd := []string{"claude"}
	if !cfg.Interactive {
		cmd = append(cmd, "-p", prompt)
	}
	cmd = append(cmd, "--output-format", "stream-json", "--verbose")

	if cfg.Model != "" {
		cmd = append(cmd, "--model", cfg.Model)
	}
	if cfg.MaxTokens > 0 {
		cmd = append(cmd, "--max-turns", fmt.Sprintf("%d", cfg.MaxTokens))
	}
	resumeSessionID := cfg.ResumeSessionID
	if resumeSessionID == "" {
		resumeSessionID = cfg.SessionID
	}
	if resumeSessionID != "" {
		cmd = append(cmd, "--resume", "--session-id", resumeSessionID)
	}
	for _, tool := range cfg.AllowedTools {
		cmd = append(cmd, "--allowedTools", tool)
	}

	return cmd, nil
}

// ParseOutput scans Claude Code NDJSON output (--output-format stream-json) for
// the result line. A result line has {"type":"result",...} with a "result" field
// containing the summary text and "subtype" indicating success or error.
func (a *ClaudeAgent) ParseOutput(output []byte) (*AgentResult, bool) {
	if len(output) == 0 {
		return nil, false
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
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

		// subtype: "success" → exit 0, anything else → exit 1
		if subtype, ok := parsed["subtype"].(string); ok {
			if subtype != "success" {
				result.ExitCode = 1
			}
			result.Metadata["subtype"] = subtype
		}

		if cost, ok := parsed["cost_usd"]; ok {
			result.Metadata["cost_usd"] = cost
		}
		if dur, ok := parsed["duration_ms"]; ok {
			result.Metadata["duration_ms"] = dur
		}

		return result, true
	}

	return nil, false
}
