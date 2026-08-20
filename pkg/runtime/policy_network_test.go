package runtime

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRenderPolicyProxyConfigDiagnosticsContainOnlyTimestampAndConnectHost(t *testing.T) {
	config, err := RenderPolicyProxyConfig([]string{"chatgpt.com"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"logformat agentd_egress %ts.%03tu\\t%>rd",
		"access_log stdio:/var/log/squid/agentd-egress.log agentd_egress",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("diagnostic proxy config missing %q:\n%s", want, config)
		}
	}
	for _, forbidden := range []string{"%ru", "%rm", "%{User-Agent}", "%{Authorization}"} {
		if strings.Contains(config, forbidden) {
			t.Fatalf("diagnostic proxy config contains payload/header field %q:\n%s", forbidden, config)
		}
	}
}

func TestWritePolicyEgressDiagnosticsFiltersAndSecuresRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	manager := &NetworkManager{DiagnosticDir: dir}
	raw := strings.Join([]string{
		"Squid startup noise",
		"1787221724.812\tchatgpt.com",
		"1787221725.001\tauth.openai.com",
		"1787221726.000\thttps://payload.invalid/path",
	}, "\n")
	path, err := manager.writePolicyEgressDiagnostics("session-private", raw)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("diagnostic dir mode=%v err=%v", info, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostic file mode=%v err=%v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("diagnostic records = %q", data)
	}
	for index, line := range lines {
		var record map[string]string
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if len(record) != 2 || record["timestamp"] == "" || record["connect_host"] == "" {
			t.Fatalf("record %d = %#v", index, record)
		}
	}
	if strings.Contains(string(data), "payload.invalid") || strings.Contains(string(data), "startup") {
		t.Fatalf("diagnostic file retained non-CONNECT data: %s", data)
	}
}

func TestPolicyEgressPreflightFailsFastWithNamedHost(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
case "$1 $2" in
  "run --rm")
    case "$*" in
      *"https://chatgpt.com/") exit 28 ;;
      *) exit 0 ;;
    esac
    ;;
esac
echo "unexpected docker command: $*" >&2
exit 2
`)
	manager := &NetworkManager{}
	spec := PolicyNetworkSpec{
		PolicyHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SessionID:  "preflight-session", AllowedHosts: []string{"api.openai.com", "chatgpt.com"},
	}
	err := manager.preflightPolicyEgress(context.Background(), spec, "agent:test")
	var egressErr *EgressError
	if !errors.As(err, &egressErr) || egressErr.Code != EgressPreflightFailed || egressErr.Host != "chatgpt.com" {
		t.Fatalf("preflight error = %#v, want named %s", err, EgressPreflightFailed)
	}
}

func TestInspectPolicyEgressFailureNamesDeniedHost(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1 $2" = "exec agentruntime-proxy-01234567-denied-s" ] && [ "$3 $4" = "sh -c" ]; then
  printf '%b\n' '1787221724.812\tchatgpt.com' '1787221725.001\tauth.openai.com'
  exit 0
fi
echo "unexpected docker command: $*" >&2
exit 2
`)
	manager := &NetworkManager{}
	spec := PolicyNetworkSpec{
		PolicyHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SessionID:  "denied-session", AllowedHosts: []string{"chatgpt.com"}, Diagnostics: true,
	}
	err := manager.inspectPolicyEgressFailure(context.Background(), spec)
	var egressErr *EgressError
	if !errors.As(err, &egressErr) || egressErr.Code != EgressDenied || egressErr.Host != "auth.openai.com" {
		t.Fatalf("inspect error = %#v, want named %s", err, EgressDenied)
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
