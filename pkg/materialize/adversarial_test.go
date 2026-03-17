package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestMaterialize_CredentialsPathTraversalContained(t *testing.T) {
	// Credentials are copied INTO the session dir by agentsessions.InitClaudeSessionDir.
	// A traversal path like "../../etc/shadow" resolves to a non-existent file,
	// so credentials are silently skipped. Verify no leaked file or mount.
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{
			CredentialsPath: "../../etc/shadow",
		},
	}, "session-12345678")
	defer result.CleanupFn()

	// No separate credentials mount should exist.
	if hasMount(result.Mounts, "/home/agent/.claude/credentials.json") {
		t.Fatal("traversal credentials path should not produce a separate mount")
	}
	// The session dir mount should exist.
	claudeMount := findMount(t, result.Mounts, "/home/agent/.claude")
	// Credentials should NOT have been copied (source doesn't exist).
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		if _, err := os.Stat(filepath.Join(claudeMount.Host, name)); err == nil {
			t.Fatalf("traversal path should not have produced %s in session dir", name)
		}
	}
}

func TestMaterialize_MemoryPathTraversalContained(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{
			MemoryPath: "../../etc/shadow",
		},
	}, "session-12345678")
	defer result.CleanupFn()

	mount := findProjectMount(t, result.Mounts)
	if mount.Host == filepath.Clean("/etc/shadow") {
		t.Fatalf("expected traversal path to be contained, got %q", mount.Host)
	}
	if !strings.HasPrefix(mount.Host, cwd+string(os.PathSeparator)) {
		t.Fatalf("expected mount host to stay under cwd %q, got %q", cwd, mount.Host)
	}
}

func TestMaterialize_DeeplyNestedSettingsJSON(t *testing.T) {
	root := map[string]any{}
	current := root
	for i := 0; i < 100; i++ {
		child := map[string]any{}
		current[fmt.Sprintf("level%d", i)] = child
		current = child
	}
	current["leaf"] = "ok"

	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{
			SettingsJSON: root,
		},
	}, "session-12345678")
	defer result.CleanupFn()

	var got map[string]any
	readJSONFile(t, filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, "settings.json"), &got)

	currentValue := any(got)
	for i := 0; i < 100; i++ {
		levelMap, ok := currentValue.(map[string]any)
		if !ok {
			t.Fatalf("expected map at depth %d, got %T", i, currentValue)
		}
		currentValue = levelMap[fmt.Sprintf("level%d", i)]
	}

	leafMap, ok := currentValue.(map[string]any)
	if !ok {
		t.Fatalf("expected leaf map, got %T", currentValue)
	}
	if leafMap["leaf"] != "ok" {
		t.Fatalf("expected leaf value ok, got %v", leafMap["leaf"])
	}
}

func TestMaterialize_LargeClaudeMDWrittenWithoutTruncation(t *testing.T) {
	payload := "BEGIN\n" + strings.Repeat("a", 10*1024*1024) + "\nEND"

	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{
			ClaudeMD: payload,
		},
	}, "session-12345678")
	defer result.CleanupFn()

	data, err := os.ReadFile(filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}

	gotHash := sha256.Sum256(data)
	wantHash := sha256.Sum256([]byte(payload))
	if gotHash != wantHash {
		t.Fatalf("expected CLAUDE.md hash %s, got %s", hex.EncodeToString(wantHash[:]), hex.EncodeToString(gotHash[:]))
	}
}

func TestMaterialize_MCPServerRejectsUnsafeURLSchemes(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{},
		MCPServers: []apischema.MCPServer{
			{Name: "js", Type: "http", URL: "javascript:alert(1)"},
			{Name: "file", Type: "http", URL: "file:///etc/passwd"},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	servers := readMCPServers(t, result)
	for _, name := range []string{"js", "file"} {
		server := servers[name].(map[string]any)
		if urlValue, ok := server["url"].(string); ok && urlValue != "" {
			t.Fatalf("expected unsafe URL for %q to be removed, got %q", name, urlValue)
		}
	}
}

func TestMaterialize_MCPServerCmdDoesNotResolveHostGateway(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{},
		MCPServers: []apischema.MCPServer{
			{
				Name: "stdio",
				Type: "stdio",
				Cmd:  []string{"mcp-serve", "${HOST_GATEWAY}"},
			},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	servers := readMCPServers(t, result)
	cmd := servers["stdio"].(map[string]any)["cmd"].([]any)
	if got := cmd[1].(string); got != "${HOST_GATEWAY}" {
		t.Fatalf("expected cmd placeholder to remain literal, got %q", got)
	}
}

func TestMaterialize_MCPServerTokenSanitized(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{},
		MCPServers: []apischema.MCPServer{
			{
				Name:  "tokenized",
				Type:  "http",
				URL:   "http://example.com",
				Token: "line1\nline2\x00tail\r",
			},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	servers := readMCPServers(t, result)
	token := servers["tokenized"].(map[string]any)["token"].(string)
	if strings.ContainsAny(token, "\n\r") || strings.ContainsRune(token, '\x00') {
		t.Fatalf("expected control characters to be removed from token, got %q", token)
	}
	if token != "line1line2tail" {
		t.Fatalf("expected sanitized token line1line2tail, got %q", token)
	}
}

func TestMaterialize_ConcurrentCallsUseUniqueTempDirs(t *testing.T) {
	const workers = 10

	type outcome struct {
		root string
		err  error
	}

	results := make(chan outcome, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			result, err := Materialize(&apischema.SessionRequest{
				Claude: &apischema.ClaudeConfig{},
			}, "shared-session", "")
			if err != nil {
				results <- outcome{err: err}
				return
			}
			defer result.CleanupFn()

			var claudeHost string
			for _, mount := range result.Mounts {
				if mount.Container == "/home/agent/.claude" {
					claudeHost = mount.Host
					break
				}
			}
			if claudeHost == "" {
				results <- outcome{err: fmt.Errorf("claude mount missing")}
				return
			}

			root := filepath.Dir(claudeHost)
			results <- outcome{root: root}
		}()
	}

	wg.Wait()
	close(results)

	seen := make(map[string]struct{}, workers)
	for result := range results {
		if result.err != nil {
			t.Fatalf("materialize concurrently: %v", result.err)
		}
		if _, exists := seen[result.root]; exists {
			t.Fatalf("duplicate temp dir detected: %q", result.root)
		}
		seen[result.root] = struct{}{}
	}

	if len(seen) != workers {
		t.Fatalf("expected %d unique temp dirs, got %d", workers, len(seen))
	}
}

func TestMaterialize_SessionIDWithPathSeparatorsUsesSafeTempDirName(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{},
	}, "../../../tmp/evil")
	defer result.CleanupFn()

	rootDir := filepath.Dir(findMount(t, result.Mounts, "/home/agent/.claude").Host)
	base := filepath.Base(rootDir)
	if strings.Contains(base, "/") || strings.Contains(base, `\`) {
		t.Fatalf("expected temp dir basename to avoid path separators, got %q", base)
	}
	if strings.Contains(base, "..") {
		t.Fatalf("expected temp dir basename to avoid traversal markers, got %q", base)
	}
	if !strings.HasPrefix(base, "agentruntime-") {
		t.Fatalf("expected temp dir basename to use agentruntime prefix, got %q", base)
	}
}

func TestMaterialize_ClaudeAndCodexMaterializeIndependently(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{
			ClaudeMD: "Claude instructions",
		},
		Codex: &apischema.CodexConfig{
			Instructions: "Codex instructions",
			ConfigTOML: map[string]any{
				"model": "gpt-5-codex",
			},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	claudeMount := findMount(t, result.Mounts, "/home/agent/.claude")
	codexMount := findMount(t, result.Mounts, "/home/agent/.codex")

	claudeData, err := os.ReadFile(filepath.Join(claudeMount.Host, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	codexData, err := os.ReadFile(filepath.Join(codexMount.Host, "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}

	if string(claudeData) != "Claude instructions" {
		t.Fatalf("unexpected CLAUDE.md contents: %q", string(claudeData))
	}
	if string(codexData) != "Codex instructions" {
		t.Fatalf("unexpected instructions contents: %q", string(codexData))
	}
}

func TestMaterialize_MCPServersWinOverConflictingMcpJSON(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{
			McpJSON: map[string]any{
				"mcpServers": map[string]any{
					"conflict": map[string]any{
						"type": "http",
						"url":  "http://old.example.com",
					},
				},
			},
		},
		MCPServers: []apischema.MCPServer{
			{
				Name: "conflict",
				Type: "http",
				URL:  "http://new.example.com",
			},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	servers := readMCPServers(t, result)
	server := servers["conflict"].(map[string]any)
	if got := server["url"].(string); got != "http://new.example.com" {
		t.Fatalf("expected MCPServers entry to win, got %q", got)
	}
}

func TestMaterialize_EmptySettingsJSONWritesEmptyObject(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{
			SettingsJSON: map[string]any{},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	data, err := os.ReadFile(filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	// Empty settings still gets skipDangerousModePermissionPrompt injected
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	if _, ok := parsed["skipDangerousModePermissionPrompt"]; !ok {
		t.Fatal("expected skipDangerousModePermissionPrompt in settings.json")
	}
}

func TestMaterialize_CleanupFnCanBeCalledTwice(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{},
	}, "session-12345678")

	rootDir := filepath.Dir(findMount(t, result.Mounts, "/home/agent/.claude").Host)
	result.CleanupFn()
	result.CleanupFn()

	if _, err := os.Stat(rootDir); !os.IsNotExist(err) {
		t.Fatalf("expected temp dir removed after double cleanup, got err=%v", err)
	}
}

func findProjectMount(t *testing.T, mounts []apischema.Mount) apischema.Mount {
	t.Helper()
	for _, mount := range mounts {
		if strings.HasPrefix(mount.Container, "/home/agent/.claude/projects/") {
			return mount
		}
	}
	t.Fatalf("project mount not found")
	return apischema.Mount{}
}

func readMCPServers(t *testing.T, result *Result) map[string]any {
	t.Helper()

	var payload map[string]any
	readJSONFile(t, filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, ".mcp.json"), &payload)

	servers, ok := payload["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcpServers object, got %T", payload["mcpServers"])
	}
	return servers
}

func TestMaterialize_MCPServerURLResolutionStillAppliesToURLsOnly(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{},
		MCPServers: []apischema.MCPServer{
			{
				Name:  "gateway",
				Type:  "http",
				URL:   "http://${HOST_GATEWAY}:8080",
				Token: "${HOST_GATEWAY}",
			},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	servers := readMCPServers(t, result)
	server := servers["gateway"].(map[string]any)
	if got := server["url"].(string); !strings.Contains(got, ResolveHostGateway()) {
		t.Fatalf("expected URL to resolve host gateway, got %q", got)
	}
	if got := server["token"].(string); got != "${HOST_GATEWAY}" {
		t.Fatalf("expected token to remain literal, got %q", got)
	}
}

func TestMaterialize_MCPServerTokenJSONRemainsObjectSafe(t *testing.T) {
	result := mustMaterialize(t, &apischema.SessionRequest{
		Claude: &apischema.ClaudeConfig{},
		MCPServers: []apischema.MCPServer{
			{
				Name:  "safe-json",
				Type:  "http",
				URL:   "http://example.com",
				Token: "prefix\nsuffix",
			},
		},
	}, "session-12345678")
	defer result.CleanupFn()

	data, err := os.ReadFile(filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("expected .mcp.json to remain valid JSON")
	}
}
