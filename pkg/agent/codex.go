package agent

import "fmt"

// CodexAgent builds commands for OpenAI Codex CLI.
type CodexAgent struct{}

func (a *CodexAgent) Name() string { return "codex" }

func (a *CodexAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	cmd := []string{"codex", "--quiet", prompt}

	if cfg.Model != "" {
		cmd = append(cmd, "--model", cfg.Model)
	}

	return cmd, nil
}

func (a *CodexAgent) ParseOutput(output []byte) (*AgentResult, bool) {
	// TODO: parse Codex CLI output for structured results
	return nil, false
}
