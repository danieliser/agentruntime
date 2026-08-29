package materialize

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/skills"
	"github.com/pelletier/go-toml/v2"
)

func testSkillBundle(t *testing.T, name, body string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{Name: name + "/SKILL.md", Mode: 0o444, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testSkillDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

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

func TestMaterialize_MCPTokenUsesEphemeralEnvironmentReference(t *testing.T) {
	const token = "scoped-session-token"
	req := &api.SessionRequest{
		Agent:  "claude",
		Claude: &api.ClaudeConfig{},
		MCPServers: []api.MCPServer{{
			Name:  "persist",
			Type:  "http",
			URL:   "http://${HOST_GATEWAY}:8803/api/v1/mcp",
			Token: token,
		}},
	}
	result := mustMaterializeWithDataDir(t, req, "session-token-reference", t.TempDir())
	defer result.CleanupFn()

	data, err := os.ReadFile(filepath.Join(result.SessionDir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("plaintext MCP token reached durable Claude config")
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	server := got["mcpServers"].(map[string]any)["persist"].(map[string]any)
	headers := server["headers"].(map[string]any)
	authorization, _ := headers["Authorization"].(string)
	if !strings.HasPrefix(authorization, "Bearer ${AGENTRUNTIME_MCP_TOKEN_") || !strings.HasSuffix(authorization, "}") {
		t.Fatalf("authorization reference = %q", authorization)
	}
	envName := strings.TrimSuffix(strings.TrimPrefix(authorization, "Bearer ${"), "}")
	if got := req.Env[envName]; got != token {
		t.Fatalf("ephemeral token env = %q, want scoped token", got)
	}
}

func TestMaterialize_CodexMergesTopLevelMCPServersWithoutPersistingToken(t *testing.T) {
	const token = "codex-scoped-session-token"
	req := &api.SessionRequest{
		Agent: "codex",
		Codex: &api.CodexConfig{},
		MCPServers: []api.MCPServer{{
			Name:  "persist",
			Type:  "http",
			URL:   "http://${HOST_GATEWAY}:8803/api/v1/mcp",
			Token: token,
		}},
	}
	result := mustMaterializeWithDataDir(t, req, "codex-mcp-reference", t.TempDir())
	defer result.CleanupFn()

	data, err := os.ReadFile(filepath.Join(result.SessionDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("plaintext MCP token reached durable Codex config")
	}

	var got map[string]any
	if err := toml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	server := got["mcp_servers"].(map[string]any)["persist"].(map[string]any)
	urlValue, _ := server["url"].(string)
	if strings.Contains(urlValue, "${HOST_GATEWAY}") || !strings.Contains(urlValue, ResolveHostGateway()) {
		t.Fatalf("resolved URL = %q", urlValue)
	}
	envName, _ := server["bearer_token_env_var"].(string)
	if !strings.HasPrefix(envName, "AGENTRUNTIME_MCP_TOKEN_") {
		t.Fatalf("bearer token env var = %q", envName)
	}
	if got := req.Env[envName]; got != token {
		t.Fatalf("ephemeral token env = %q, want scoped token", got)
	}
}

func TestMaterialize_CodexStdioMCPUsesNativeCommandShape(t *testing.T) {
	req := &api.SessionRequest{
		Agent: "codex",
		Codex: &api.CodexConfig{},
		MCPServers: []api.MCPServer{{
			Name: "worker", Type: "stdio", Cmd: []string{"node", "server.js", "--stdio"},
		}},
	}
	result := mustMaterializeWithDataDir(t, req, "codex-stdio-mcp", t.TempDir())
	defer result.CleanupFn()

	data, err := os.ReadFile(filepath.Join(result.SessionDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := toml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	server := got["mcp_servers"].(map[string]any)["worker"].(map[string]any)
	if command, ok := server["command"].(string); !ok || command != "node" {
		t.Fatalf("Codex MCP command = %#v, want scalar node", server["command"])
	}
	args, ok := server["args"].([]any)
	if !ok || len(args) != 2 || args[0] != "server.js" || args[1] != "--stdio" {
		t.Fatalf("Codex MCP args = %#v", server["args"])
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

func TestMaterialize_RestrictedPolicyDoesNotCopyAmbientCredentialsOrContext(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(dataDir, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "credentials", "claude-credentials.json"), []byte(`{"secret":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "credentials", "codex-auth.json"), []byte(`{"secret":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := &api.ExecutionPolicy{
		Version: "1.0", Workspace: "ephemeral", Filesystem: "read_only", Network: "public_https",
		AllowedTools: []string{"web_search"}, MCPServers: []string{}, HostMounts: []string{}, ApprovalPolicy: "never",
	}

	claude, err := Materialize(&api.SessionRequest{Agent: "claude", Context: "clean", AutoDiscover: false, ExecutionPolicy: policy}, "restricted-claude", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	claudeDir := findMount(t, claude.Mounts, "/home/agent/.claude").Host
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		if _, err := os.Stat(filepath.Join(claudeDir, name)); !os.IsNotExist(err) {
			t.Fatalf("restricted Claude materialized ambient %s: %v", name, err)
		}
	}
	var mcp map[string]any
	readJSONFile(t, filepath.Join(claudeDir, ".mcp.json"), &mcp)
	if servers, ok := mcp["mcpServers"].(map[string]any); !ok || len(servers) != 0 {
		t.Fatalf("restricted Claude MCP config = %#v", mcp)
	}
	assertPrivatePathMode(t, claudeDir, 0o700)
	assertPrivatePathMode(t, filepath.Join(claudeDir, ".mcp.json"), 0o600)

	codex, err := Materialize(&api.SessionRequest{Agent: "codex", Context: "clean", AutoDiscover: false, ExecutionPolicy: policy}, "restricted-codex", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	codexDir := findMount(t, codex.Mounts, "/home/agent/.codex").Host
	if _, err := os.Stat(filepath.Join(codexDir, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("restricted Codex materialized ambient auth.json: %v", err)
	}
	assertPrivatePathMode(t, codexDir, 0o700)
	assertPrivatePathMode(t, filepath.Join(codexDir, "config.toml"), 0o600)
}

func TestMaterialize_RestrictedCodexConsumesExplicitAuthGrant(t *testing.T) {
	dataDir := t.TempDir()
	auth := `{"auth_mode":"chatgpt","tokens":{"access_token":"one-session-secret"}}`
	request := &api.SessionRequest{
		Agent: "codex", Context: "clean", AutoDiscover: false,
		ExecutionPolicy: &api.ExecutionPolicy{
			Version: "1.0", Workspace: "ephemeral", Filesystem: "read_only", Network: "public_https",
			AllowedTools: []string{"web_search"}, MCPServers: []string{}, HostMounts: []string{}, ApprovalPolicy: "never",
		},
		Env:          map[string]string{api.CodexAuthJSONEnv: auth, "VISIBLE_SETTING": "retained"},
		SecretGrants: []string{api.CodexAuthJSONEnv},
	}
	result, err := Materialize(request, "explicit-codex-auth", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	codexDir := findMount(t, result.Mounts, "/home/agent/.codex").Host
	actual, err := os.ReadFile(filepath.Join(codexDir, "auth.json"))
	if err != nil {
		t.Fatalf("read explicitly granted auth: %v", err)
	}
	if string(actual) != auth {
		t.Fatal("explicitly granted auth bytes changed")
	}
	assertPrivatePathMode(t, filepath.Join(codexDir, "auth.json"), 0o600)
	if _, exists := request.Env[api.CodexAuthJSONEnv]; exists {
		t.Fatal("consumed Codex auth remained in provider environment")
	}
	if request.Env["VISIBLE_SETTING"] != "retained" {
		t.Fatal("materialization removed an unrelated environment value")
	}
}

func assertPrivatePathMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if actual := info.Mode().Perm(); actual != want {
		t.Fatalf("mode %s = %04o, want %04o", path, actual, want)
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
	var config map[string]any
	if err := toml.Unmarshal(configData, &config); err != nil {
		t.Fatalf("parse config.toml: %v", err)
	}
	if config["model"] != "gpt-5-codex" || config["quiet"] != true {
		t.Fatalf("unexpected config.toml values: %+v", config)
	}

	instructions, err := os.ReadFile(filepath.Join(mount.Host, "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if string(instructions) != "Follow repo conventions.\n" {
		t.Fatalf("unexpected instructions: %q", string(instructions))
	}
}

func TestMaterialize_CodexAppliesRequestedModelControlsWithoutHighDefault(t *testing.T) {
	request := &api.SessionRequest{
		Model: "gpt-5.6-sol", Effort: "ultra", Fast: true,
		Codex: &api.CodexConfig{ConfigTOML: map[string]any{"quiet": true}},
	}
	result := mustMaterializeWithDataDir(t, request, "session-controls", t.TempDir())
	defer result.CleanupFn()

	mount := findMount(t, result.Mounts, "/home/agent/.codex")
	configData, err := os.ReadFile(filepath.Join(mount.Host, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	var config map[string]any
	if err := toml.Unmarshal(configData, &config); err != nil {
		t.Fatalf("parse config.toml: %v", err)
	}
	for key, expected := range map[string]any{
		"model": "gpt-5.6-sol", "model_reasoning_effort": "ultra", "service_tier": "priority",
	} {
		if config[key] != expected {
			t.Errorf("requested Codex config %s=%v, want %v", key, config[key], expected)
		}
	}
	if len(request.Codex.ConfigTOML) != 1 || request.Codex.ConfigTOML["quiet"] != true {
		t.Fatalf("materializer mutated caller config: %+v", request.Codex.ConfigTOML)
	}
}

func TestMaterialize_CodexPreservesAutoDiscoveredNestedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostConfig := `model = "gpt-5.6-sol"

[features]
memories = true
multi_agent = true

[plugins."github@openai-curated"]
enabled = true
`
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(hostConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Agent: "codex", WorkDir: project, Codex: &api.CodexConfig{},
	}, "session-nested-config", t.TempDir())
	defer result.CleanupFn()
	configData, err := os.ReadFile(filepath.Join(findMount(t, result.Mounts, "/home/agent/.codex").Host, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := toml.Unmarshal(configData, &config); err != nil {
		t.Fatalf("materialized config.toml is invalid: %v\n%s", err, configData)
	}
	features, ok := config["features"].(map[string]any)
	if !ok || features["memories"] != true || features["multi_agent"] != true {
		t.Fatalf("nested features were not preserved: %+v", config)
	}
	plugins, ok := config["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("nested plugins table was not preserved: %+v", config)
	}
	github, ok := plugins["github@openai-curated"].(map[string]any)
	if !ok || github["enabled"] != true {
		t.Fatalf("quoted plugin key was not preserved: %+v", plugins)
	}
	if _, flattened := config["memories"]; flattened {
		t.Fatalf("nested memories key leaked to top level: %+v", config)
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

func TestMaterialize_ContentAddressedSkillsAsReadOnlyNestedMounts(t *testing.T) {
	dataDir := t.TempDir()
	bundle := testSkillBundle(t, "persist-comms", "skill body")
	digest := testSkillDigest(bundle)
	store := skills.NewStore(filepath.Join(dataDir, "skill-bundles"))
	if _, err := store.Put(digest, bytes.NewReader(bundle)); err != nil {
		t.Fatalf("put bundle: %v", err)
	}
	for _, tc := range []struct{ agent, root string }{{"claude", "/home/agent/.claude"}, {"codex", "/home/agent/.codex"}} {
		t.Run(tc.agent, func(t *testing.T) {
			result := mustMaterializeWithDataDir(t, &api.SessionRequest{Agent: tc.agent, Skills: &api.SkillSet{
				SchemaVersion: "1.0", Mode: "replace", Entries: []api.SkillRef{{Name: "persist-comms", BundleSHA256: digest}},
			}}, "skills-"+tc.agent, dataDir)
			mount := findMount(t, result.Mounts, tc.root+"/skills")
			if mount.Mode != "ro" {
				t.Fatalf("skill mount mode = %q", mount.Mode)
			}
			body, err := os.ReadFile(filepath.Join(mount.Host, "persist-comms", "SKILL.md"))
			if err != nil || string(body) != "skill body" {
				t.Fatalf("skill body = %q, err %v", body, err)
			}
		})
	}
}

func TestMaterialize_EmptySkillSetMountsReadOnlyProviderSkillRoot(t *testing.T) {
	for _, tc := range []struct{ agent, root string }{{"claude", "/home/agent/.claude"}, {"codex", "/home/agent/.codex"}} {
		t.Run(tc.agent, func(t *testing.T) {
			result := mustMaterializeWithDataDir(t, &api.SessionRequest{
				Agent:  tc.agent,
				Skills: &api.SkillSet{SchemaVersion: "1.0", Mode: "replace", Entries: []api.SkillRef{}},
			}, "empty-skills-"+tc.agent, t.TempDir())
			mount := findMount(t, result.Mounts, tc.root+"/skills")
			if mount.Mode != "ro" {
				t.Fatalf("skill root mount mode = %q", mount.Mode)
			}
			entries, err := os.ReadDir(mount.Host)
			if err != nil || len(entries) != 0 {
				t.Fatalf("empty skill root entries = %v, err %v", entries, err)
			}
		})
	}
}

func TestMaterialize_SkillReplaceRemovesUnrequestedSessionEntries(t *testing.T) {
	dataDir := t.TempDir()
	bundle := testSkillBundle(t, "persist-comms", "skill body")
	digest := testSkillDigest(bundle)
	store := skills.NewStore(filepath.Join(dataDir, "skill-bundles"))
	if _, err := store.Put(digest, bytes.NewReader(bundle)); err != nil {
		t.Fatal(err)
	}
	request := &api.SessionRequest{Agent: "codex", Skills: &api.SkillSet{
		SchemaVersion: "1.0", Mode: "replace", Entries: []api.SkillRef{{Name: "persist-comms", BundleSHA256: digest}},
	}}
	first := mustMaterializeWithDataDir(t, request, "replace-skills", dataDir)
	skillRoot := filepath.Join(first.SessionDir, "skills")
	if err := os.MkdirAll(filepath.Join(skillRoot, "stale-skill"), 0o700); err != nil {
		t.Fatal(err)
	}
	second := mustMaterializeWithDataDir(t, request, "replace-skills", dataDir)
	if _, err := os.Lstat(filepath.Join(second.SessionDir, "skills", "stale-skill")); !os.IsNotExist(err) {
		t.Fatalf("replace mode retained unrequested skill: %v", err)
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
