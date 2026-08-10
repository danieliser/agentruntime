package api

import (
	"fmt"
	"strconv"

	"github.com/danieliser/agentruntime/pkg/agent"
)

// resolvedNativeExecution is the single ACT-1001 provider launch resolution.
// Callers persist the request separately, but no admission path may rebuild
// provider flags independently from this result.
type resolvedNativeExecution struct {
	Config  agent.AgentConfig
	Command []string
}

func resolveNativeExecution(
	request SessionRequest,
	implementation agent.Agent,
	workDir string,
	providerID string,
) (resolvedNativeExecution, error) {
	const op = "resolve_native_execution"
	if implementation == nil {
		return resolvedNativeExecution{}, fmt.Errorf("%s: agent implementation is required", op)
	}
	config := agent.AgentConfig{
		Model: request.Model, WorkDir: workDir, Env: request.Env,
		Interactive: request.Interactive, NativeStream: true,
		SessionID: request.SessionID, ResumeSessionID: providerID,
		Effort: request.Effort, Fast: request.Fast,
	}
	if request.Claude != nil {
		config.MaxTokens = request.Claude.MaxTurns
		config.AllowedTools = append([]string(nil), request.Claude.AllowedTools...)
	}
	command, err := implementation.BuildCmd("", config)
	if err != nil {
		return resolvedNativeExecution{}, fmt.Errorf("%s: build %s command: %w", op, implementation.Name(), err)
	}
	if _, codex := implementation.(*agent.CodexAgent); codex {
		command = []string{"codex", "app-server", "--listen", "stdio://", "--strict-config"}
		if config.Model != "" {
			command = append(command, "-c", "model="+strconv.Quote(config.Model))
		}
		if config.Effort != "" {
			command = append(command, "-c", "model_reasoning_effort="+strconv.Quote(config.Effort))
		}
		if config.Fast {
			command = append(command, "-c", `service_tier="priority"`)
		}
	}
	return resolvedNativeExecution{Config: config, Command: command}, nil
}
