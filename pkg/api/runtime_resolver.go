package api

import (
	"fmt"
	"strconv"

	"github.com/danieliser/agentruntime/pkg/agent"
	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/durable"
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
	if request.Runtime == "local" && request.ProviderPermissions != nil && request.ProviderPermissions.Mode == "restricted" {
		switch implementation.(type) {
		case *agent.CodexAgent, *agent.ClaudeAgent:
		default:
			return resolvedNativeExecution{}, durable.NewError(durable.CodeExecutionPolicyUnsupported, op, "agent driver cannot enforce the read-confined local profile", nil)
		}
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
	if request.ProviderPermissions != nil && request.ProviderPermissions.Mode == "restricted" {
		config.IsolateConfig = request.Runtime == "local"
		config.PermissionMode = "default"
		if request.ProviderPermissions.ApprovalPolicy == "never" {
			config.PermissionMode = "dontAsk"
		}
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
		if request.ProviderPermissions != nil {
			sandbox := "danger-full-access"
			if request.ProviderPermissions.Mode == "restricted" {
				sandbox = "workspace-write"
			}
			command = append(command, "-c", "approval_policy="+strconv.Quote(request.ProviderPermissions.ApprovalPolicy))
			if request.Runtime == "local" && request.ProviderPermissions.Mode == "restricted" {
				command = append(command, "-c", "default_permissions="+strconv.Quote(apischema.RestrictedLocalCodexProfile))
			} else {
				command = append(command, "-c", "sandbox_mode="+strconv.Quote(sandbox))
			}
		}

		if request.Runtime == "local" && request.ProviderPermissions != nil && request.ProviderPermissions.Mode == "restricted" {
			isolation, err := restrictedLocalCodexArgs(workDir)
			if err != nil {
				return resolvedNativeExecution{}, err
			}
			command = append(command, isolation...)
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
				"-c", "features.respect_system_proxy=true",
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
