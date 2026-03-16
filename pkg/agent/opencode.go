package agent

import "fmt"

// OpenCodeAgent builds commands for the OpenCode CLI.
type OpenCodeAgent struct{}

func (a *OpenCodeAgent) Name() string { return "opencode" }

func (a *OpenCodeAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	// TODO: verify opencode CLI flags once stable
	cmd := []string{"opencode", "run", prompt}

	if cfg.Model != "" {
		cmd = append(cmd, "--model", cfg.Model)
	}

	return cmd, nil
}

func (a *OpenCodeAgent) ParseOutput(output []byte) (*AgentResult, bool) {
	// TODO: parse OpenCode output for structured results
	return nil, false
}
