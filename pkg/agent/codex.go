package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

// CodexAgent builds commands for OpenAI Codex CLI.
type CodexAgent struct{}

func (a *CodexAgent) Name() string { return "codex" }

func (a *CodexAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	cmd := []string{"codex", "exec", "--json", "--full-auto", "--skip-git-repo-check", prompt}

	if cfg.Model != "" {
		cmd = append(cmd, "--model", cfg.Model)
	}
	resumeSessionID := cfg.ResumeSessionID
	if resumeSessionID == "" {
		resumeSessionID = cfg.SessionID
	}
	if resumeSessionID != "" {
		cmd = append(cmd, "--session", resumeSessionID)
	}

	return cmd, nil
}

// ParseOutput scans Codex CLI NDJSON output for completion events.
// Codex emits either "message.completed" or "response.completed" events
// containing the final output.
func (a *CodexAgent) ParseOutput(output []byte) (*AgentResult, bool) {
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

		eventType, _ := parsed["type"].(string)

		switch eventType {
		case "message.completed":
			result := &AgentResult{
				Metadata: make(map[string]any),
			}
			if content, ok := parsed["content"].(string); ok {
				result.Summary = content
			}
			return result, true

		case "response.completed":
			result := &AgentResult{
				Metadata: make(map[string]any),
			}
			// Navigate response.output[].content[].text
			if resp, ok := parsed["response"].(map[string]any); ok {
				if outputs, ok := resp["output"].([]any); ok {
					for _, o := range outputs {
						if msg, ok := o.(map[string]any); ok {
							if contents, ok := msg["content"].([]any); ok {
								for _, c := range contents {
									if item, ok := c.(map[string]any); ok {
										if text, ok := item["text"].(string); ok {
											result.Summary = text
										}
									}
								}
							}
						}
					}
				}
			}
			return result, true
		}
	}

	return nil, false
}
