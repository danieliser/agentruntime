package api

import (
	"encoding/json"
	"testing"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func TestRestrictedLocalReviewReadNetworkScope(t *testing.T) {
	req := SessionRequest{Agent: "codex", Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
	policy := nativePolicy(req)
	if policy.NetworkAccess {
		t.Error("local network is enabled")
	}
	adapter, _ := nativeprotocol.NewAdapter(nativeprotocol.ProviderCodex)
	frames, err := adapter.Encode(nativeprotocol.Input{Kind: nativeprotocol.InputPrompt, Text: "fixture", ProviderID: "thread", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body.Params["sandboxPolicy"]; exists {
		t.Error("legacy sandbox override can discard read confinement")
	}
	if string(body.Params["permissions"]) != `"agentd-local-v1"` {
		t.Error("named read-confined permission profile not selected")
	}
}

func TestRestrictedLocalReviewRejectClaudeAndMounts(t *testing.T) {
	for _, agentName := range []string{"claude", "codex"} {
		req := SessionRequest{Agent: agentName, Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "never"}}
		if agentName == "codex" {
			req.Mounts = []Mount{{Host: "/extra", Container: "/extra"}}
		}
		if err := validateProviderPermissions(&req, "local"); !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
			t.Fatalf("unsafe %s accepted: %v", agentName, err)
		}
	}
}

func TestProxyEnvironmentSecretsNeverPersist(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "https_proxy", "All_Proxy"} {
		req := SessionRequest{Agent: "codex", Env: map[string]string{name: "http://fixture-user:fixture-password@proxy.invalid:8080"}}
		if _, _, _, err := durableRequestManifest(req, "local"); err == nil {
			t.Fatalf("%s persisted without a secret grant", name)
		}
		req.SecretGrants = []string{name}
		manifest, _, _, err := durableRequestManifest(req, "local")
		if err != nil {
			t.Fatal(err)
		}
		var stored struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(manifest, &stored); err != nil {
			t.Fatal(err)
		}
		if _, exists := stored.Env[name]; exists {
			t.Fatalf("%s persisted in plaintext", name)
		}
	}
}
