package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

const (
	LegacyExecutionPolicyVersion = "2.0"
	ExecutionPolicyVersion       = "2.1"
)

var (
	DefaultResourceLimits = ResourceLimits{MemoryBytes: 2 << 30, CPUCores: 2, PIDs: 256, OpenFiles: 1024}
	MaximumResourceLimits = DefaultResourceLimits
	MinimumResourceLimits = ResourceLimits{MemoryBytes: 64 << 20, CPUCores: 0.1, PIDs: 16, OpenFiles: 64}
)

var executionPolicyEgressHosts = map[string]map[string]struct{}{
	"codex": {
		"auth.openai.com": {},
		"chatgpt.com":     {},
	},
	"claude": {
		"api.anthropic.com": {},
	},
}

var executionToolEgressHosts = map[string][]string{
	"web_search": {"api.openai.com"},
}

type resolvedExecutionPolicy struct {
	Policy *ExecutionPolicy
	Hash   string
}

// resolveExecutionPolicy canonicalizes an explicit caller grant and rejects
// every authority AgentD cannot currently prove. A nil policy deliberately
// remains the documented legacy profile for v2.0 client compatibility.
func resolveExecutionPolicy(request *SessionRequest, runtimeName string) (resolvedExecutionPolicy, error) {
	const op = "resolve_execution_policy"
	if _, err := apischema.ExplicitCodexAuthJSON(request); err != nil {
		return resolvedExecutionPolicy{}, durable.NewError(durable.CodeInvalidArgument, op, "invalid explicit credential grant", err)
	}
	if request == nil || request.ExecutionPolicy == nil {
		return resolvedExecutionPolicy{}, nil
	}
	policy := *request.ExecutionPolicy
	if policy.Version == "" {
		policy.Version = ExecutionPolicyVersion
	}
	unsupported := func(message string) (resolvedExecutionPolicy, error) {
		return resolvedExecutionPolicy{}, durable.NewError(durable.CodeExecutionPolicyUnsupported, op, message, nil)
	}
	if policy.Version != ExecutionPolicyVersion && policy.Version != LegacyExecutionPolicyVersion {
		return unsupported(fmt.Sprintf("execution policy version %q is unsupported", policy.Version))
	}
	if runtimeName != "docker" {
		return unsupported("execution policy v1 requires the Docker runtime")
	}
	if policy.Workspace != "ephemeral" {
		return unsupported("execution policy v1 supports only workspace=ephemeral")
	}
	if policy.WorkspaceRetention == "" {
		policy.WorkspaceRetention = "terminal_receipt"
	}
	if policy.WorkspaceRetention != "terminal_receipt" {
		return unsupported("execution policy v1 supports only workspace_retention=terminal_receipt")
	}
	if policy.Filesystem != "read_only" && policy.Filesystem != "workspace_write" {
		return unsupported("filesystem must be read_only or workspace_write")
	}
	if policy.Network != "public_https" {
		return unsupported("execution policy v1 supports only managed public_https egress")
	}
	if policy.ApprovalPolicy != "never" {
		return unsupported("execution policy v1 supports only approval_policy=never")
	}
	if policy.Version == LegacyExecutionPolicyVersion {
		if policy.Resources != nil {
			return unsupported("execution policy v2.0 does not define caller-selected resource limits")
		}
	} else {
		if policy.Resources == nil {
			limits := DefaultResourceLimits
			policy.Resources = &limits
		}
		if err := validateResourceLimits(*policy.Resources); err != nil {
			return resolvedExecutionPolicy{}, err
		}
	}
	if request.Container != nil && (request.Container.Memory != "" || request.Container.CPUs != 0) {
		return unsupported("container resource overrides cannot widen an execution policy")
	}

	tools, err := canonicalUnique(policy.AllowedTools)
	if err != nil {
		return unsupported("allowed_tools must contain unique non-empty names")
	}
	for _, tool := range tools {
		if tool != "web_search" {
			return unsupported(fmt.Sprintf("tool %q is not supported by execution policy v1", tool))
		}
	}
	policy.AllowedTools = tools
	egressHosts, err := canonicalUnique(policy.EgressAllowlist)
	if err != nil {
		return unsupported("egress_allowlist must contain unique non-empty exact hosts")
	}
	ownedHosts := executionPolicyEgressHosts[request.Agent]
	if ownedHosts == nil {
		return unsupported(fmt.Sprintf("agent %q has no managed egress endpoints", request.Agent))
	}
	permittedHosts := make(map[string]struct{}, len(ownedHosts)+1)
	for host := range ownedHosts {
		permittedHosts[host] = struct{}{}
	}
	for _, tool := range tools {
		for _, host := range executionToolEgressHosts[tool] {
			permittedHosts[host] = struct{}{}
		}
	}
	for _, host := range egressHosts {
		if host != strings.ToLower(host) || strings.ContainsAny(host, "*:/") || strings.HasPrefix(host, ".") || net.ParseIP(host) != nil {
			return unsupported(fmt.Sprintf("egress host %q is not a lowercase exact DNS host", host))
		}
		if _, permitted := permittedHosts[host]; !permitted {
			return unsupported(fmt.Sprintf("egress host %q is not a managed endpoint for agent %q", host, request.Agent))
		}
	}
	policy.EgressAllowlist = egressHosts
	policy.MCPServers = emptyIfNil(policy.MCPServers)
	policy.HostMounts = emptyIfNil(policy.HostMounts)
	if len(policy.MCPServers) != 0 || len(request.MCPServers) != 0 {
		return unsupported("execution policy v1 does not grant MCP servers")
	}
	if len(policy.HostMounts) != 0 || len(request.EffectiveMounts()) != 0 {
		return unsupported("ephemeral workspace does not grant host mounts")
	}
	if request.PersistSession {
		return unsupported("ephemeral workspace cannot use a persistent session volume")
	}
	if request.ResumeSession != "" {
		return unsupported("ephemeral workspace cannot resume retained provider state")
	}
	if request.Lifecycle != nil || request.Team != nil {
		return unsupported("execution policy v1 does not grant lifecycle hooks or agent teams")
	}
	if request.Claude != nil && (len(request.Claude.AllowedTools) != 0 || len(request.Claude.SettingsJSON) != 0 || len(request.Claude.McpJSON) != 0 ||
		request.Claude.ClaudeMD != "" || request.Claude.CredentialsPath != "" || request.Claude.MemoryPath != "") {
		return unsupported("Claude provider configuration cannot widen the execution policy")
	}
	if request.Codex != nil && (len(request.Codex.ConfigTOML) != 0 || request.Codex.Instructions != "" || request.Codex.ApprovalMode != "") {
		return unsupported("Codex provider configuration cannot widen the execution policy")
	}
	if request.Context != "" && request.Context != "clean" {
		return unsupported("execution policy v1 requires clean context")
	}
	request.Context = "clean"
	request.AutoDiscover = false
	request.ExecutionPolicy = &policy

	raw, err := json.Marshal(policy)
	if err != nil {
		return resolvedExecutionPolicy{}, durable.NewError(durable.CodeInvalidArgument, op, "encode canonical execution policy", err)
	}
	digest := sha256.Sum256(raw)
	return resolvedExecutionPolicy{Policy: request.ExecutionPolicy, Hash: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func validateResourceLimits(limits ResourceLimits) error {
	const op = "resolve_execution_policy"
	if limits.MemoryBytes < MinimumResourceLimits.MemoryBytes || limits.CPUCores < MinimumResourceLimits.CPUCores ||
		limits.PIDs < MinimumResourceLimits.PIDs || limits.OpenFiles < MinimumResourceLimits.OpenFiles ||
		math.IsNaN(limits.CPUCores) || math.IsInf(limits.CPUCores, 0) {
		return durable.NewError(durable.CodeResourceLimitExceeded, op, "requested resource limits are below AgentD minimums", nil)
	}
	if limits.MemoryBytes > MaximumResourceLimits.MemoryBytes || limits.CPUCores > MaximumResourceLimits.CPUCores ||
		limits.PIDs > MaximumResourceLimits.PIDs || limits.OpenFiles > MaximumResourceLimits.OpenFiles {
		return durable.NewError(durable.CodeResourceLimitExceeded, op, "requested resource limits exceed AgentD maximums", nil)
	}
	return nil
}

func canonicalUnique(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	sort.Strings(result)
	for index, value := range result {
		if value == "" || (index > 0 && result[index-1] == value) {
			return nil, fmt.Errorf("values must be unique and non-empty")
		}
	}
	return emptyIfNil(result), nil
}

func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func manifestExecutionPolicy(manifest json.RawMessage) (*ExecutionPolicy, string) {
	var stored struct {
		Policy *ExecutionPolicy `json:"execution_policy"`
		Hash   string           `json:"execution_policy_hash"`
	}
	if json.Unmarshal(manifest, &stored) != nil {
		return nil, ""
	}
	return stored.Policy, stored.Hash
}

func manifestPolicyHash(manifest json.RawMessage) string {
	_, hash := manifestExecutionPolicy(manifest)
	return hash
}

func nativePolicy(request SessionRequest) nativeprotocol.InputPolicy {
	if request.ExecutionPolicy == nil {
		return nativeprotocol.InputPolicy{}
	}
	return nativeprotocol.InputPolicy{
		Enforced: true, ApprovalPolicy: request.ExecutionPolicy.ApprovalPolicy,
		Filesystem:    request.ExecutionPolicy.Filesystem,
		NetworkAccess: request.ExecutionPolicy.Network == "public_https",
	}
}
