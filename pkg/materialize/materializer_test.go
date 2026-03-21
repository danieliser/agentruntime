package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestMaterialize_ClaudeWritesSettingsJSON(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			SettingsJSON: map[string]any{
				"theme": "light",
				"count": float64(2),
			},
		},
	}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	var got map[string]any
	readJSONFile(t, filepath.Join(result.Mounts[0].Host, "settings.json"), &got)

	if got["theme"] != "light" {
		t.Fatalf("expected theme light, got %v", got["theme"])
	}
	if got["count"] != float64(2) {
		t.Fatalf("expected count 2, got %v", got["count"])
	}
}

func TestMaterialize_ClaudeWritesClaudeMD(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			ClaudeMD: "# team instructions\nuse ripgrep\n",
		},
	}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	data, err := os.ReadFile(filepath.Join(result.Mounts[0].Host, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}

	if string(data) != "# team instructions\nuse ripgrep\n" {
		t.Fatalf("unexpected CLAUDE.md contents: %q", string(data))
	}
}

func TestMaterialize_ClaudeWritesMcpJSON_MergesServers(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			McpJSON: map[string]any{
				"other": "value",
				"mcpServers": map[string]map[string]any{
					"existing": map[string]any{
						"type": "stdio",
						"cmd":  []any{"old-server"},
					},
					"replace": map[string]any{
						"type": "http",
						"url":  "http://old",
					},
				},
			},
		},
		MCPServers: []api.MCPServer{
			{
				Name: "replace",
				Type: "http",
				URL:  "http://new",
			},
			{
				Name: "added",
				Type: "stdio",
				Cmd:  []string{"mcp-added", "--serve"},
			},
		},
	}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	var got map[string]any
	readJSONFile(t, filepath.Join(result.Mounts[0].Host, ".mcp.json"), &got)

	if got["other"] != "value" {
		t.Fatalf("expected top-level field preserved, got %v", got["other"])
	}

	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["existing"]; !ok {
		t.Fatal("expected existing server to remain")
	}
	if servers["replace"].(map[string]any)["url"] != "http://new" {
		t.Fatalf("expected replacement server URL http://new, got %v", servers["replace"].(map[string]any)["url"])
	}
	addedServer := servers["added"].(map[string]any)
	if addedServer["command"] != "mcp-added" {
		t.Fatalf("expected command 'mcp-added', got %v", addedServer["command"])
	}
	addedArgs := addedServer["args"].([]any)
	if len(addedArgs) != 1 || addedArgs[0] != "--serve" {
		t.Fatalf("unexpected added args: %#v", addedArgs)
	}
}

func TestMaterialize_HostGatewayResolved(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			McpJSON: map[string]any{
				"mcpServers": map[string]any{
					"gateway": map[string]any{
						"type": "http",
						"url":  "http://${HOST_GATEWAY}:8080",
					},
				},
			},
		},
		MCPServers: []api.MCPServer{
			{
				Name: "added",
				Type: "http",
				URL:  "http://${HOST_GATEWAY}:9000",
			},
		},
	}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	var got map[string]any
	readJSONFile(t, filepath.Join(result.Mounts[0].Host, ".mcp.json"), &got)

	servers := got["mcpServers"].(map[string]any)
	for _, name := range []string{"gateway", "added"} {
		url := servers[name].(map[string]any)["url"].(string)
		if strings.Contains(url, "${HOST_GATEWAY}") {
			t.Fatalf("expected HOST_GATEWAY to be resolved in %q", url)
		}
		if !strings.Contains(url, ResolveHostGateway()) {
			t.Fatalf("expected resolved gateway in %q", url)
		}
	}
}

func TestMaterialize_CredentialsCopiedIntoSessionDir(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(credPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write credentials file: %v", err)
	}
	t.Setenv("MATERIALIZE_CREDENTIALS_FILE", credPath)

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			CredentialsPath: "${MATERIALIZE_CREDENTIALS_FILE}",
		},
	}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	mount := findMount(t, result.Mounts, "/home/agent/.claude")
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		data, err := os.ReadFile(filepath.Join(mount.Host, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(data) != "{}" {
			t.Fatalf("unexpected credentials in %s: %q", name, string(data))
		}
	}
	if hasMount(result.Mounts, "/home/agent/.claude/credentials.json") {
		t.Fatal("expected credentials to be copied into the Claude session dir, not mounted separately")
	}
}

func TestMaterialize_MemoryPathMounted(t *testing.T) {
	dir := t.TempDir()
	memoryDir := filepath.Join(dir, "memory")
	if err := os.Mkdir(memoryDir, 0o755); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	t.Setenv("MATERIALIZE_MEMORY_DIR", memoryDir)

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			MemoryPath: "${MATERIALIZE_MEMORY_DIR}",
		},
	}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	hash := sha256.Sum256([]byte(memoryDir))
	wantContainer := "/home/agent/.claude/projects/" + hex.EncodeToString(hash[:])[:16]
	mount := findMount(t, result.Mounts, wantContainer)
	if mount.Mode != "ro" {
		t.Fatalf("expected ro mount, got %q", mount.Mode)
	}
	if mount.Host != memoryDir {
		t.Fatalf("expected host path %q, got %q", memoryDir, mount.Host)
	}
}

func TestMaterialize_CleanupDeletesTempDir(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "session-12345678", "")

	rootDir := filepath.Dir(result.Mounts[0].Host)
	if _, err := os.Stat(rootDir); err != nil {
		t.Fatalf("expected temp dir to exist before cleanup: %v", err)
	}

	result.CleanupFn()

	if _, err := os.Stat(rootDir); !os.IsNotExist(err) {
		t.Fatalf("expected temp dir removed, got err=%v", err)
	}
}

func TestMaterialize_CodexWritesConfigAndInstructions(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Codex: &api.CodexConfig{
			ConfigTOML: map[string]any{
				"model": "gpt-5-codex",
				"quiet": true,
			},
			Instructions: "Follow repo conventions.\n",
		},
	}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	mount := findMount(t, result.Mounts, "/home/agent/.codex")
	configData, err := os.ReadFile(filepath.Join(mount.Host, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	config := string(configData)
	if !strings.Contains(config, "model = \"gpt-5-codex\"") {
		t.Fatalf("expected model in config.toml, got %q", config)
	}
	if !strings.Contains(config, "quiet = true") {
		t.Fatalf("expected quiet in config.toml, got %q", config)
	}

	instructions, err := os.ReadFile(filepath.Join(mount.Host, "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if string(instructions) != "Follow repo conventions.\n" {
		t.Fatalf("unexpected instructions: %q", string(instructions))
	}
}

func TestMaterialize_NilAgentConfig(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{}, "session-12345678", t.TempDir())
	defer result.CleanupFn()

	if len(result.Mounts) != 0 {
		t.Fatalf("expected no mounts, got %d", len(result.Mounts))
	}
	if result.CleanupFn == nil {
		t.Fatal("expected cleanup function")
	}
}

func TestMaterialize_EmptySessionID(t *testing.T) {
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "", "")
	defer result.CleanupFn()

	if len(result.Mounts) == 0 {
		t.Fatal("expected Claude mount to be created")
	}
}

func TestMaterialize_UsesAgentSessionDir(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "session-12345678"

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
		Codex:  &api.CodexConfig{},
	}, sessionID, dataDir)
	defer result.CleanupFn()

	claudeMount := findMount(t, result.Mounts, "/home/agent/.claude")
	if want := filepath.Join(dataDir, "claude-sessions", sessionID); claudeMount.Host != want {
		t.Fatalf("expected Claude mount host %q, got %q", want, claudeMount.Host)
	}

	codexMount := findMount(t, result.Mounts, "/home/agent/.codex")
	if want := filepath.Join(dataDir, "codex-sessions", sessionID); codexMount.Host != want {
		t.Fatalf("expected Codex mount host %q, got %q", want, codexMount.Host)
	}
}

func TestMaterialize_SessionDirPersistsAfterCleanup(t *testing.T) {
	dataDir := t.TempDir()

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			ClaudeMD: "persistent session",
		},
	}, "session-12345678", dataDir)

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("expected session dir to exist before cleanup: %v", err)
	}

	result.CleanupFn()

	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("expected session dir to persist after cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "CLAUDE.md")); err != nil {
		t.Fatalf("expected session contents to persist after cleanup: %v", err)
	}
}

func mustMaterializeWithDataDir(t *testing.T, req *api.SessionRequest, sessionID, dataDir string) *Result {
	t.Helper()
	result, err := Materialize(req, sessionID, dataDir)
	if err != nil {
		t.Fatalf("Materialize returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.CleanupFn == nil {
		t.Fatal("expected cleanup function")
	}
	return result
}

func mustMaterialize(t *testing.T, req *api.SessionRequest, sessionID string) *Result {
	t.Helper()
	return mustMaterializeWithDataDir(t, req, sessionID, "")
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

func findMount(t *testing.T, mounts []api.Mount, container string) api.Mount {
	t.Helper()
	for _, mount := range mounts {
		if mount.Container == container {
			return mount
		}
	}
	t.Fatalf("mount for container %q not found", container)
	return api.Mount{}
}

func hasMount(mounts []api.Mount, container string) bool {
	for _, mount := range mounts {
		if mount.Container == container {
			return true
		}
	}
	return false
}
