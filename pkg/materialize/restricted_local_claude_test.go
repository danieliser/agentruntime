package materialize

import (
	"encoding/json"
	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"path/filepath"
	"slices"
	"testing"
)

func TestRestrictedLocalClaudeSettings(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "project")
	home := filepath.Join(root, "state", "local-home")
	req := &apischema.SessionRequest{WorkDir: work, ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
	data, err := RestrictedLocalClaudeSettings(req, home, filepath.Join(home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg RestrictedClaudeSettings
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.DisableAllHooks || !cfg.Sandbox.Enabled || !cfg.Sandbox.FailIfUnavailable || cfg.Sandbox.AllowUnsandboxedCommands || cfg.Sandbox.AutoAllowBashIfSandboxed || len(cfg.Sandbox.ExcludedCommands) > 0 {
		t.Fatal("sandbox escape enabled")
	}
	if len(cfg.Permissions.Allow) > 0 || !cfg.Permissions.BlockReadsOutsideWorkingDirectories {
		t.Fatal("permission widening")
	}
	for _, tool := range []string{"Read", "Edit", "Write"} {
		if !slices.Contains(cfg.Permissions.Deny, tool+"(/"+filepath.Join(home, ".claude")+"/**)") {
			t.Error("provider directory deny missing", tool)
		}
	}
	for _, tool := range []string{"Edit", "Write", "Bash"} {
		if !slices.Contains(cfg.Permissions.Ask, tool) {
			t.Error("missing approval", tool)
		}
	}
	for _, path := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		if !slices.Contains(cfg.Sandbox.Filesystem.DenyRead, path) || !slices.Contains(cfg.Sandbox.Filesystem.DenyWrite, path) {
			t.Error("missing shared temp deny", path)
		}
	}
	if !slices.Contains(cfg.Sandbox.Filesystem.AllowRead, work) || !slices.Contains(cfg.Sandbox.Filesystem.AllowWrite, home) || !slices.Contains(cfg.Sandbox.Filesystem.DenyRead, "/") {
		t.Fatal("wrong filesystem boundary")
	}
	if len(cfg.Sandbox.Network.AllowedDomains) > 0 || !slices.Contains(cfg.Sandbox.Network.DeniedDomains, "*") || cfg.Sandbox.Network.AllowLocalBinding || cfg.Sandbox.Network.AllowAllUnixSockets {
		t.Fatal("network widened")
	}
}
