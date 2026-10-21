package api

import (
	"github.com/danieliser/agentruntime/pkg/agent"
	"strings"
	"testing"
)

func TestProviderPermissionsRestrictCodingWithoutResearchToolLimits(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		request := SessionRequest{Agent: provider, Runtime: "docker", WorkDir: "/tmp/project", ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
		if err := validateProviderPermissions(&request, "docker"); err != nil {
			t.Fatal(err)
		}
		result, err := resolveNativeExecution(request, agent.DefaultRegistry().Get(provider), "/workspace", "")
		if err != nil {
			t.Fatal(err)
		}
		cmd := strings.Join(result.Command, " ")
		if strings.Contains(cmd, "dangerously-skip") || strings.Contains(cmd, "--disable shell_tool") {
			t.Fatalf("coding permissions lost: %s", cmd)
		}
		if provider == "claude" && !strings.Contains(cmd, "--permission-mode default") {
			t.Fatal(cmd)
		}
		if provider == "codex" && (!strings.Contains(cmd, `approval_policy="on-request"`) || !strings.Contains(cmd, `sandbox_mode="workspace-write"`)) {
			t.Fatal(cmd)
		}
		policy := nativePolicy(request)
		if !policy.Enforced || policy.Filesystem != "workspace_write" || policy.ApprovalPolicy != "on-request" {
			t.Fatalf("native policy %+v", policy)
		}
	}
}
func TestProviderPermissionsRejectWidening(t *testing.T) {
	tests := []struct {
		name, runtime string
		mutate        func(*SessionRequest)
	}{
		{"unsupported runtime", "local-pipe", func(r *SessionRequest) {}},
		{"unknown mode", "docker", func(r *SessionRequest) { r.ProviderPermissions.Mode = "auto" }},
		{"unknown approval", "docker", func(r *SessionRequest) { r.ProviderPermissions.ApprovalPolicy = "allow" }},
		{"file-only Docker unsupported", "docker", func(r *SessionRequest) { r.ProviderPermissions.Tools = "file_only" }},
		{"file-only unrestricted unsupported", "local", func(r *SessionRequest) {
			r.ProviderPermissions.Mode = "unrestricted"
			r.ProviderPermissions.ApprovalPolicy = "never"
			r.ProviderPermissions.Tools = "file_only"
		}},
		{"file-only Codex unsupported", "local", func(r *SessionRequest) { r.Agent = "codex"; r.ProviderPermissions.Tools = "file_only" }},
		{"container host network", "docker", func(r *SessionRequest) { r.Container = &ContainerConfig{Network: "host"} }},
		{"container security overrides", "docker", func(r *SessionRequest) { r.Container = &ContainerConfig{SecurityOpt: []string{"seccomp=unconfined"}} }},
		{"extra host mounts", "docker", func(r *SessionRequest) { r.Volumes = []string{"/Users:/host"} }},
		{"ambient context", "docker", func(r *SessionRequest) { r.Context = "shared" }},
		{"claude permission overrides", "docker", func(r *SessionRequest) {
			r.Claude = &ClaudeConfig{SettingsJSON: map[string]any{"permissions": map[string]any{"defaultMode": "bypassPermissions"}}}
		}},
		{"codex sandbox overrides", "docker", func(r *SessionRequest) { r.Codex = &CodexConfig{ApprovalMode: "full-auto"} }},
		{"research policy conflict", "docker", func(r *SessionRequest) { r.ExecutionPolicy = &ExecutionPolicy{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := SessionRequest{Agent: "claude", WorkDir: "/tmp/project", ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
			tt.mutate(&r)
			if validateProviderPermissions(&r, tt.runtime) == nil {
				t.Fatal("unexpected widening accepted")
			}
		})
	}
}
