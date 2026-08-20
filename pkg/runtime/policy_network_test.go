package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestPolicyNetworkSpecUsesHashScopedNames(t *testing.T) {
	spec := PolicyNetworkSpec{
		PolicyHash:   "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SessionID:    "session-abcdef12",
		AllowedHosts: []string{"api.openai.com", "chatgpt.com"},
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := spec.NetworkName(), "agentruntime-policy-01234567-session-"; got != want {
		t.Fatalf("network name = %q, want %q", got, want)
	}
	if got, want := spec.ProxyContainerName(), "agentruntime-proxy-01234567-session-"; got != want {
		t.Fatalf("proxy name = %q, want %q", got, want)
	}
}

func TestPolicyProxyConfigIsOwnerPrivate(t *testing.T) {
	manager := &NetworkManager{DataDir: t.TempDir()}
	spec := PolicyNetworkSpec{
		PolicyHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SessionID:  "session-private", AllowedHosts: []string{"api.openai.com"},
	}
	path, err := manager.writePolicyProxyConfig(spec)
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode %s = %04o, want %04o", target, got, want)
		}
	}
}

func TestRestrictedPolicyRejectsCallerProxyEnvironmentBypass(t *testing.T) {
	runtime := NewDockerRuntime(DockerConfig{Image: "fixture:latest"})
	_, err := runtime.prepareRun(SpawnConfig{
		SessionID: "proxy-bypass", Cmd: []string{"true"},
		Request: &apischema.SessionRequest{Env: map[string]string{"NO_PROXY": "*"}, ExecutionPolicy: &apischema.ExecutionPolicy{
			Version: "2.0", Workspace: "ephemeral", Filesystem: "read_only", Network: "public_https",
			EgressAllowlist: []string{}, AllowedTools: []string{}, ApprovalPolicy: "never",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved for AgentD-managed egress") {
		t.Fatalf("prepareRun proxy bypass error = %v", err)
	}
}

func TestRenderPolicyProxyConfigAllowsOnlyExactHostsAndDisablesLogs(t *testing.T) {
	config, err := RenderPolicyProxyConfig([]string{"api.openai.com", "chatgpt.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{"dstdomain api.openai.com", "dstdomain chatgpt.com"} {
		if !strings.Contains(config, exact) {
			t.Fatalf("proxy config missing %q:\n%s", exact, config)
		}
	}
	for _, forbidden := range []string{"dstdomain .openai.com", "*.openai.com", "access.log", "cache.log", "http_access allow all"} {
		if strings.Contains(config, forbidden) {
			t.Fatalf("proxy config contains forbidden %q:\n%s", forbidden, config)
		}
	}
	if !strings.Contains(config, "access_log none") || !strings.Contains(config, "http_access deny all") {
		t.Fatalf("proxy config is not non-logging default-deny:\n%s", config)
	}
}

func TestRenderPolicyProxyConfigEmptyAllowlistDeniesEverything(t *testing.T) {
	config, err := RenderPolicyProxyConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config, "http_access allow") || !strings.Contains(config, "http_access deny all") {
		t.Fatalf("empty allowlist config does not deny everything:\n%s", config)
	}
}

func TestPolicyNetworkSpecRejectsInvalidHashAndHosts(t *testing.T) {
	for _, spec := range []PolicyNetworkSpec{
		{PolicyHash: "sha256:short", SessionID: "session", AllowedHosts: []string{"api.openai.com"}},
		{PolicyHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", SessionID: "session", AllowedHosts: []string{"*.openai.com"}},
		{PolicyHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", SessionID: "session", AllowedHosts: []string{"1.1.1.1"}},
	} {
		if err := spec.Validate(); err == nil {
			t.Fatalf("Validate(%+v) unexpectedly passed", spec)
		}
	}
}
