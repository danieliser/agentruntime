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
	if request.ExecutionPolicy != nil {
		config.EnforcePolicy = true
		config.PermissionMode = "dontAsk"
		config.AllowedTools = providerTools(request.Agent, request.ExecutionPolicy.AllowedTools)
	}
	if request.StructuredOutput != nil {
		config.JSONSchema = append([]byte(nil), request.StructuredOutput.JSONSchema...)
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
		if request.ExecutionPolicy != nil {
			filesystemMode := "read-only"
			if request.ExecutionPolicy.Filesystem == "workspace_write" {
				filesystemMode = "workspace-write"
			}
			command = append(command,
				"-c", `approval_policy="never"`,
				"-c", "sandbox_mode="+strconv.Quote(filesystemMode),
				"-c", "tools.web_search="+strconv.FormatBool(hasCanonicalTool(request.ExecutionPolicy.AllowedTools, "web_search")),
			)
			for _, feature := range []string{
				"shell_tool", "unified_exec", "js_repl", "image_generation", "view_image",
				"computer_use", "browser_use", "plugins", "enable_mcp_apps", "skill_search", "multi_agent",
			} {
				command = append(command, "--disable", feature)
			}
		}
	}
	return resolvedNativeExecution{Config: config, Command: command}, nil
}

func providerTools(agentName string, tools []string) []string {
	resolved := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == "web_search" && agentName == "claude" {
			resolved = append(resolved, "WebSearch")
		}
	}
	return resolved
}

func hasCanonicalTool(tools []string, target string) bool {
	for _, tool := range tools {
		if tool == target {
			return true
		}
	}
	return false
}
