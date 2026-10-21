package api

import (
	"fmt"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"path/filepath"
	"reflect"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/durable"
)

func validateProviderPermissions(request *SessionRequest, runtimeName string) error {
	if request.ProviderPermissions == nil {
		return nil
	}
	p := request.ProviderPermissions
	reject := func(reason string) error {
		return durable.NewError(durable.CodeExecutionPolicyUnsupported, "provider_permissions", reason, nil)
	}
	if p.Mode != "restricted" && p.Mode != "unrestricted" {
		return reject("mode must be restricted or unrestricted")
	}
	if p.ApprovalPolicy != "never" && p.ApprovalPolicy != "on-request" {
		return reject("approval_policy must be never or on-request")
	}
	if request.Agent != "claude" && request.Agent != "codex" {
		return reject("provider permissions support only Claude and Codex")
	}
	if request.ExecutionPolicy != nil {
		return reject("provider_permissions cannot be combined with execution_policy")
	}
	if p.Tools != "" && (p.Tools != "file_only" || runtimeName != "local" || p.Mode != "restricted" || request.Agent != "claude") {
		return reject("provider_permissions.tools=file_only is supported only for restricted local Claude")
	}
	if p.Mode == "unrestricted" {
		if p.ApprovalPolicy != "never" {
			return reject("unrestricted mode requires approval_policy=never")
		}
		return nil
	}
	if runtimeName != "docker" && runtimeName != "local" {
		return reject("restricted coding requires Docker or local runtime")
	}
	if request.WorkDir == "" {
		return reject("restricted coding requires an explicit project work_dir")
	}
	if len(request.Mounts) > 0 || len(request.Volumes) > 0 {
		return reject("restricted coding allows only the work_dir host mount")
	}
	if runtimeName == "docker" && request.Container != nil && ((request.Container.Network != "" && request.Container.Network != "bridge") || len(request.Container.SecurityOpt) > 0) {
		return reject("restricted coding cannot override container network or security controls")
	}
	if request.Context != "" && request.Context != "clean" {
		return reject("restricted coding requires clean context")
	}
	if request.Lifecycle != nil || request.Team != nil {
		return reject("restricted coding does not grant lifecycle hooks or teams")
	}
	if request.Claude != nil && (len(request.Claude.SettingsJSON) > 0 || request.Claude.MemoryPath != "" || request.Claude.Bare) {
		return reject("restricted coding does not accept Claude settings, memory or bare-mode overrides")
	}
	if request.Codex != nil && (len(request.Codex.ConfigTOML) > 0 || request.Codex.ApprovalMode != "") {
		return reject("restricted coding does not accept Codex config or approval overrides")
	}
	if runtimeName == "local" {
		if request.Agent == "claude" && (request.Skills != nil || request.Trace != nil) {
			return reject("restricted local Claude does not accept caller skills or trace configuration")
		}
		if err := runtime.CheckRestrictedLocalProviderPermissions(request.Agent, p.Tools); err != nil {
			if request.Agent == "codex" && p.Tools == "" {
				return reject("provider/version/platform has no qualified read-confined local sandbox")
			}
			return err
		}
		if !filepath.IsAbs(request.WorkDir) {
			return reject("restricted local coding requires an absolute project work_dir")
		}
		if (request.Agent != "claude" && request.Claude != nil) || (request.Agent == "claude" && (request.Codex != nil || (request.Claude != nil && !reflect.DeepEqual(*request.Claude, ClaudeConfig{})))) {
			return reject("restricted local coding does not accept caller provider configuration")
		}
		if len(request.MCPServers) > 0 {
			return reject("restricted local coding does not grant external MCP processes or servers")
		}
		for name := range request.Env {
			if !apischema.RestrictedLocalEnvAllowed(name) {
				return reject(fmt.Sprintf("environment variable %q is not supported for restricted local coding", name))
			}
		}
	}
	request.Runtime = runtimeName
	// Provider configuration is explicit. Discovery must not import owner config,
	// credentials, MCP servers, permissions or sibling mounts from the host.
	request.Context = "clean"
	request.AutoDiscover = false
	return nil
}
