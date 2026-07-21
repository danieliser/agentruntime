package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// GrokAgent builds commands for the native xAI grok CLI.
//
// grok is single-turn headless only (`-p`): there is no pipe-friendly
// interactive protocol, so Interactive requests are rejected. The sidecar
// runtimes only use the binary name from BuildCmd and construct the real
// argv in cmd/sidecar/grok.go; the full argv here serves the legacy
// local-pipe path and command preview.
type GrokAgent struct{}

func (a *GrokAgent) Name() string { return "grok" }

func (a *GrokAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
	if cfg.Interactive {
		return nil, fmt.Errorf("grok does not support interactive mode: single-turn prompt mode only")
	}
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	cmd := []string{"grok", "--always-approve", "--output-format", "streaming-json"}
	if cfg.Model != "" {
		cmd = append(cmd, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		cmd = append(cmd, "--reasoning-effort", cfg.Effort)
	}
	if cfg.ResumeSessionID != "" {
		cmd = append(cmd, "--resume", cfg.ResumeSessionID)
	}
	cmd = append(cmd, "-p", prompt)

	return cmd, nil
}

// ParseOutput scans grok streaming-json output. Text deltas accumulate into
// the summary; the {"type":"end"} terminal event carries usage and cost.
func (a *GrokAgent) ParseOutput(output []byte) (*AgentResult, bool) {
	if len(output) == 0 {
		return nil, false
	}

	var summary strings.Builder
	var result *AgentResult

	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal(line, &parsed); err != nil {
			continue
		}

		switch parsed["type"] {
		case "text":
			if text, ok := parsed["data"].(string); ok {
				summary.WriteString(text)
			}
		case "end":
			result = &AgentResult{
				Metadata: make(map[string]any),
			}
			if stopReason, ok := parsed["stopReason"].(string); ok {
				result.Metadata["stop_reason"] = stopReason
				if stopReason != "EndTurn" {
					result.ExitCode = 1
				}
			}
			if sessionID, ok := parsed["sessionId"].(string); ok {
				result.Metadata["session_id"] = sessionID
			}
			if cost, ok := parsed["total_cost_usd"]; ok {
				result.Metadata["cost_usd"] = cost
			}
			if turns, ok := parsed["num_turns"]; ok {
				result.Metadata["num_turns"] = turns
			}
		}
	}

	if result == nil {
		return nil, false
	}
	result.Summary = summary.String()
	return result, true
}
