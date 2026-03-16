package materialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danieliser/agentruntime/pkg/api"
)

// --- TDD tests for the materializer. Written RED before implementation. ---

func TestMaterialize_ClaudeSettingsJSON_WrittenCorrectly(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		Claude: &api.ClaudeConfig{
			SettingsJSON: map[string]any{
				"permissions": map[string]any{
					"allow": []any{"Read", "Write"},
				},
			},
		},
	}

	result, err := Materialize(req, "test-session-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()

	// Find the .claude mount.
	var claudeMount *api.Mount
	for i := range result.Mounts {
		if strings.HasSuffix(result.Mounts[i].Container, ".claude") ||
			result.Mounts[i].Container == "/root/.claude" {
			claudeMount = &result.Mounts[i]
			break
		}
	}
	if claudeMount == nil {
		t.Fatal("expected a mount for .claude directory")
	}

	// Read settings.json from the host path.
	settingsPath := filepath.Join(claudeMount.Host, "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}

	perms, ok := parsed["permissions"]
	if !ok {
		t.Fatal("expected 'permissions' key in settings.json")
	}
	_ = perms
}

func TestMaterialize_ClaudeMD_Written(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		Claude: &api.ClaudeConfig{
			ClaudeMD: "# Custom Instructions\nDo the thing.",
		},
	}

	result, err := Materialize(req, "test-md-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()

	var claudeMount *api.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Container == "/root/.claude" {
			claudeMount = &result.Mounts[i]
			break
		}
	}
	if claudeMount == nil {
		t.Fatal("expected .claude mount")
	}

	mdPath := filepath.Join(claudeMount.Host, "CLAUDE.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("failed to read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(data), "Custom Instructions") {
		t.Fatalf("CLAUDE.md content mismatch: %q", string(data))
	}
}

func TestMaterialize_MCPServers_MergedIntoMcpJSON(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		Claude: &api.ClaudeConfig{
			McpJSON: map[string]any{
				"existing-server": map[string]any{"url": "http://existing:9000"},
			},
		},
		MCPServers: []api.MCPServer{
			{
				Name: "persist",
				Type: "http",
				URL:  "http://localhost:8801/mcp",
			},
		},
	}

	result, err := Materialize(req, "test-mcp-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()

	var claudeMount *api.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Container == "/root/.claude" {
			claudeMount = &result.Mounts[i]
			break
		}
	}
	if claudeMount == nil {
		t.Fatal("expected .claude mount")
	}

	mcpPath := filepath.Join(claudeMount.Host, ".mcp.json")
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("failed to read .mcp.json: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf(".mcp.json not valid JSON: %v", err)
	}

	// Both the existing server and the MCPServer entry should be present.
	if _, ok := parsed["existing-server"]; !ok {
		t.Fatal("expected 'existing-server' in merged .mcp.json")
	}
	if _, ok := parsed["persist"]; !ok {
		t.Fatal("expected 'persist' from MCPServers in merged .mcp.json")
	}
}

func TestMaterialize_HostGateway_ResolvedInMCPURLs(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		Claude: &api.ClaudeConfig{},
		MCPServers: []api.MCPServer{
			{
				Name: "daemon",
				Type: "http",
				URL:  "http://${HOST_GATEWAY}:8801/mcp",
			},
		},
	}

	result, err := Materialize(req, "test-gw-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()

	var claudeMount *api.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Container == "/root/.claude" {
			claudeMount = &result.Mounts[i]
			break
		}
	}
	if claudeMount == nil {
		t.Fatal("expected .claude mount")
	}

	mcpPath := filepath.Join(claudeMount.Host, ".mcp.json")
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("failed to read .mcp.json: %v", err)
	}

	if strings.Contains(string(data), "${HOST_GATEWAY}") {
		t.Fatalf("${HOST_GATEWAY} was NOT resolved in .mcp.json: %s", data)
	}
}

func TestMaterialize_CredentialsPath_AddedAsROMount(t *testing.T) {
	// Create a fake credentials file.
	tmpCreds := filepath.Join(t.TempDir(), "credentials.json")
	os.WriteFile(tmpCreds, []byte(`{"token":"fake"}`), 0o600)

	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		Claude: &api.ClaudeConfig{
			CredentialsPath: tmpCreds,
		},
	}

	result, err := Materialize(req, "test-creds-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()

	var foundCreds bool
	for _, m := range result.Mounts {
		if strings.Contains(m.Container, "credentials.json") && m.Mode == "ro" {
			foundCreds = true
			break
		}
	}
	if !foundCreds {
		t.Fatalf("expected ro mount for credentials.json, got mounts: %+v", result.Mounts)
	}
}

func TestMaterialize_CleanupDeletesTempDir(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		Claude: &api.ClaudeConfig{
			ClaudeMD: "test cleanup",
		},
	}

	result, err := Materialize(req, "test-cleanup-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}

	// Grab a host path before cleanup.
	var hostDir string
	for _, m := range result.Mounts {
		if m.Container == "/root/.claude" {
			hostDir = m.Host
			break
		}
	}
	if hostDir == "" {
		t.Fatal("no .claude mount found")
	}

	// Verify it exists.
	if _, err := os.Stat(hostDir); err != nil {
		t.Fatalf("temp dir should exist before cleanup: %v", err)
	}

	// Run cleanup.
	result.CleanupFn()

	// Parent temp dir should be gone.
	parentDir := filepath.Dir(hostDir)
	if _, err := os.Stat(parentDir); !os.IsNotExist(err) {
		t.Fatalf("temp dir should be deleted after cleanup, but still exists: %s", parentDir)
	}
}

func TestMaterialize_NilAgentConfig_EmptyMounts(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		// No Claude or Codex config.
	}

	result, err := Materialize(req, "test-nil-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()

	if len(result.Mounts) != 0 {
		t.Fatalf("expected 0 mounts with nil agent config, got %d: %+v", len(result.Mounts), result.Mounts)
	}
}

func TestMaterialize_CodexWritesInstructions(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "codex",
		Prompt: "test",
		Codex: &api.CodexConfig{
			Instructions: "Always write tests first.",
			ApprovalMode: "full-auto",
		},
	}

	result, err := Materialize(req, "test-codex-001")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()

	var codexMount *api.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Container == "/root/.codex" {
			codexMount = &result.Mounts[i]
			break
		}
	}
	if codexMount == nil {
		t.Fatal("expected .codex mount")
	}

	instrPath := filepath.Join(codexMount.Host, "instructions.md")
	data, err := os.ReadFile(instrPath)
	if err != nil {
		t.Fatalf("failed to read instructions.md: %v", err)
	}
	if !strings.Contains(string(data), "Always write tests first") {
		t.Fatalf("instructions.md content mismatch: %q", string(data))
	}
}

func TestMaterialize_EmptySessionID_NoPanic(t *testing.T) {
	req := &api.SessionRequest{
		Agent:  "claude",
		Prompt: "test",
		Claude: &api.ClaudeConfig{},
	}

	// Should not panic with empty session ID.
	result, err := Materialize(req, "")
	if err != nil {
		t.Fatalf("Materialize failed: %v", err)
	}
	defer result.CleanupFn()
}
