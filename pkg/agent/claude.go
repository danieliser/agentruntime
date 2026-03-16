package agent

import "fmt"

// ClaudeAgent builds commands for Claude Code CLI.
type ClaudeAgent struct{}

func (a *ClaudeAgent) Name() string { return "claude" }

func (a *ClaudeAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	cmd := []string{"claude", "-p", prompt, "--output-format", "stream-json"}

	if cfg.Model != "" {
		cmd = append(cmd, "--model", cfg.Model)
	}
	if cfg.MaxTokens > 0 {
		cmd = append(cmd, "--max-turns", fmt.Sprintf("%d", cfg.MaxTokens))
	}
	if cfg.SessionID != "" {
		cmd = append(cmd, "--session-id", cfg.SessionID, "--resume")
	}
	for _, tool := range cfg.AllowedTools {
		cmd = append(cmd, "--allowedTools", tool)
	}

	return cmd, nil
}

func (a *ClaudeAgent) ParseOutput(output []byte) (*AgentResult, bool) {
	// TODO: parse Claude Code NDJSON output for structured results
	return nil, false
}
