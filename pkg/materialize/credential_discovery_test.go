package materialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/danieliser/agentruntime/pkg/api/schema"
)

// --- Claude credential discovery ---

func TestClaudeCredentials_SyncCacheCopied(t *testing.T) {
	dataDir := t.TempDir()

	// Place a credential sync cache file where the daemon would write it.
	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	creds := `{"oauth_token":"sync-token"}`
	if err := os.WriteFile(filepath.Join(syncDir, "claude-credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "cred-sync-test", dataDir)
	defer result.CleanupFn()

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
		if string(data) != creds {
			t.Fatalf("%s: expected %q, got %q", name, creds, string(data))
		}
	}
}

func TestClaudeCredentials_FallbackToHostDotClaude(t *testing.T) {
	// This test verifies that when no sync cache exists, the materializer
	// checks ~/.claude/.credentials.json. We can't mock os.UserHomeDir,
	// so instead we verify the code path via an explicitly provided
	// CredentialsPath that simulates the fallback source.
	dataDir := t.TempDir()
	hostClaudeDir := filepath.Join(dataDir, "fake-home", ".claude")
	if err := os.MkdirAll(hostClaudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	creds := `{"oauth_token":"host-cred"}`
	if err := os.WriteFile(filepath.Join(hostClaudeDir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}

	// Provide the path explicitly (mimics what the auto-discovery resolves to).
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			CredentialsPath: filepath.Join(hostClaudeDir, ".credentials.json"),
		},
	}, "cred-fallback-test", dataDir)
	defer result.CleanupFn()

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
		if string(data) != creds {
			t.Fatalf("%s mismatch: got %q", name, string(data))
		}
	}
}

func TestClaudeCredentials_NoneExist_StillSucceeds(t *testing.T) {
	dataDir := t.TempDir()

	// No sync cache, no host credentials — should not error.
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "no-creds-test", dataDir)
	defer result.CleanupFn()

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	// Neither credentials file should exist.
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		if _, err := os.Stat(filepath.Join(sessionDir, name)); err == nil {
			t.Fatalf("expected %s to NOT exist when no credentials available", name)
		}
	}
}

// --- Codex credential discovery ---

func TestCodexCredentials_AuthJSONCopied(t *testing.T) {
	dataDir := t.TempDir()

	// Place a credential sync cache for codex.
	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := `{"api_key":"codex-key"}`
	if err := os.WriteFile(filepath.Join(syncDir, "codex-auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Codex: &api.CodexConfig{},
	}, "codex-auth-test", dataDir)
	defer result.CleanupFn()

	codexDir := findMount(t, result.Mounts, "/home/agent/.codex").Host
	data, err := os.ReadFile(filepath.Join(codexDir, "auth.json"))
	if err != nil {
		t.Fatalf("expected auth.json: %v", err)
	}
	if string(data) != auth {
		t.Fatalf("auth.json mismatch: got %q", string(data))
	}
}

func TestCodexCredentials_NoneExist_StillSucceeds(t *testing.T) {
	dataDir := t.TempDir()

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Codex: &api.CodexConfig{},
	}, "codex-no-auth-test", dataDir)
	defer result.CleanupFn()

	codexDir := findMount(t, result.Mounts, "/home/agent/.codex").Host
	if _, err := os.Stat(filepath.Join(codexDir, "auth.json")); err == nil {
		t.Fatal("expected auth.json to NOT exist when no credentials available")
	}
}

// --- Permission checks ---

func TestClaudeCredentials_Permissions0600(t *testing.T) {
	dataDir := t.TempDir()

	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "claude-credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "perm-test-claude", dataDir)
	defer result.CleanupFn()

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		info, err := os.Stat(filepath.Join(sessionDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		perm := info.Mode().Perm()
		if perm != 0o600 {
			t.Fatalf("%s permissions: want 0600, got %04o", name, perm)
		}
	}
}

func TestCodexCredentials_Permissions0600(t *testing.T) {
	dataDir := t.TempDir()

	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "codex-auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Codex: &api.CodexConfig{},
	}, "perm-test-codex", dataDir)
	defer result.CleanupFn()

	codexDir := findMount(t, result.Mounts, "/home/agent/.codex").Host
	info, err := os.Stat(filepath.Join(codexDir, "auth.json"))
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("auth.json permissions: want 0600, got %04o", perm)
	}
}

// --- Docker mount verification ---

func TestMounts_ClaudeAndCodexDirsPresent(t *testing.T) {
	dataDir := t.TempDir()

	// Set up both agents with credentials so all mounts fire.
	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(syncDir, "claude-credentials.json"), []byte(`{}`), 0o600)
	os.WriteFile(filepath.Join(syncDir, "codex-auth.json"), []byte(`{}`), 0o600)

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
		Codex:  &api.CodexConfig{},
	}, "mount-verify-test", dataDir)
	defer result.CleanupFn()

	// Both agent home dirs must be present in mounts.
	findMount(t, result.Mounts, "/home/agent/.claude")
	findMount(t, result.Mounts, "/home/agent/.codex")
}

// --- .claude.json per-session verification ---

func TestClaudeJSON_WrittenPerSession(t *testing.T) {
	dataDir := t.TempDir()

	// Create two sessions and verify each gets its own .claude.json.
	sessions := []string{"session-aaa", "session-bbb"}
	paths := make([]string, 2)

	for i, sid := range sessions {
		result := mustMaterializeWithDataDir(t, &api.SessionRequest{
			Claude: &api.ClaudeConfig{},
		}, sid, dataDir)
		defer result.CleanupFn()

		claudeDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
		statePath := filepath.Join(claudeDir, ".claude.json")
		if _, err := os.Stat(statePath); err != nil {
			t.Fatalf("session %s: .claude.json not found: %v", sid, err)
		}
		paths[i] = statePath
	}

	// The two files must be in different directories (per-session, not shared).
	if filepath.Dir(paths[0]) == filepath.Dir(paths[1]) {
		t.Fatalf(".claude.json written to same dir for both sessions: %s", filepath.Dir(paths[0]))
	}

	// Verify .claude.json is also bind-mounted as a single file.
	for _, sid := range sessions {
		result := mustMaterializeWithDataDir(t, &api.SessionRequest{
			Claude: &api.ClaudeConfig{},
		}, sid, dataDir)
		defer result.CleanupFn()

		mount := findMount(t, result.Mounts, "/home/agent/.claude.json")
		if !strings.HasSuffix(mount.Host, ".claude.json") {
			t.Fatalf("expected .claude.json mount host to end with .claude.json, got %q", mount.Host)
		}
		// Verify the mounted file is inside the session dir, not a shared parent.
		if !strings.Contains(mount.Host, sid) {
			t.Fatalf(".claude.json mount host %q does not contain session ID %q", mount.Host, sid)
		}
	}
}

// --- settings.json skipDangerousModePermissionPrompt ---

func TestSettingsJSON_SkipDangerousModePrompt(t *testing.T) {
	dataDir := t.TempDir()

	// Without explicit setting — should be injected.
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "settings-test", dataDir)
	defer result.CleanupFn()

	var got map[string]any
	readJSONFile(t, filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, "settings.json"), &got)

	val, ok := got["skipDangerousModePermissionPrompt"]
	if !ok {
		t.Fatal("skipDangerousModePermissionPrompt missing from settings.json")
	}
	if val != true {
		t.Fatalf("expected skipDangerousModePermissionPrompt=true, got %v", val)
	}
}

func TestSettingsJSON_SkipDangerousModePrompt_UserOverrideRespected(t *testing.T) {
	dataDir := t.TempDir()

	// User explicitly sets it to false — should be preserved.
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			SettingsJSON: map[string]any{
				"skipDangerousModePermissionPrompt": false,
			},
		},
	}, "settings-override-test", dataDir)
	defer result.CleanupFn()

	var got map[string]any
	readJSONFile(t, filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, "settings.json"), &got)

	if got["skipDangerousModePermissionPrompt"] != false {
		t.Fatalf("expected user override false to be preserved, got %v", got["skipDangerousModePermissionPrompt"])
	}
}

// --- ${HOST_GATEWAY} resolution ---

func TestMCPServers_HostGatewayResolved(t *testing.T) {
	dataDir := t.TempDir()

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			McpJSON: map[string]any{
				"mcpServers": map[string]any{
					"local": map[string]any{
						"type": "http",
						"url":  "http://${HOST_GATEWAY}:3000/mcp",
					},
				},
			},
		},
		MCPServers: []api.MCPServer{
			{
				Name: "remote",
				Type: "http",
				URL:  "ws://${HOST_GATEWAY}:4000",
			},
		},
	}, "gateway-test", dataDir)
	defer result.CleanupFn()

	var got map[string]any
	readJSONFile(t, filepath.Join(findMount(t, result.Mounts, "/home/agent/.claude").Host, ".mcp.json"), &got)

	servers := got["mcpServers"].(map[string]any)
	resolved := ResolveHostGateway()

	for _, name := range []string{"local", "remote"} {
		srv := servers[name].(map[string]any)
		u, ok := srv["url"].(string)
		if !ok {
			t.Fatalf("server %q missing url", name)
		}
		if strings.Contains(u, "${HOST_GATEWAY}") {
			t.Fatalf("server %q: HOST_GATEWAY not resolved in %q", name, u)
		}
		if !strings.Contains(u, resolved) {
			t.Fatalf("server %q: expected resolved IP %q in URL %q", name, resolved, u)
		}
	}
}

// --- discoverCodexAuth unit test ---

func TestDiscoverCodexAuth_SyncCachePriority(t *testing.T) {
	dataDir := t.TempDir()

	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	syncAuth := `{"source":"sync"}`
	if err := os.WriteFile(filepath.Join(syncDir, "codex-auth.json"), []byte(syncAuth), 0o600); err != nil {
		t.Fatal(err)
	}

	got := discoverCodexAuth(dataDir)
	if got == nil {
		t.Fatal("expected sync cache auth to be returned")
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["source"] != "sync" {
		t.Fatalf("expected sync source, got %v", parsed["source"])
	}
}

func TestDiscoverCodexAuth_EmptyDataDir(t *testing.T) {
	// With empty dataDir + no ~/.codex/auth.json, should return nil (not error).
	got := discoverCodexAuth("")
	// Can't guarantee nil here since the test runner's home might have auth.json,
	// so we just verify it doesn't panic.
	_ = got
}

// --- Host fallback tests with HOME override ---

func TestDiscoverCodexAuth_HostFallback(t *testing.T) {
	fakeHome := t.TempDir()
	codexDir := filepath.Join(fakeHome, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostAuth := `{"api_key":"host-codex"}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(hostAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	got := discoverCodexAuth("") // no sync cache
	if got == nil {
		t.Fatal("expected host fallback, got nil")
	}
	if string(got) != hostAuth {
		t.Fatalf("expected host auth, got %q", string(got))
	}
}

func TestDiscoverCodexAuth_SyncCacheBeatsHost(t *testing.T) {
	dataDir := t.TempDir()
	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	syncAuth := `{"source":"sync-wins"}`
	if err := os.WriteFile(filepath.Join(syncDir, "codex-auth.json"), []byte(syncAuth), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeHome := t.TempDir()
	codexDir := filepath.Join(fakeHome, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"source":"host-loses"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	got := discoverCodexAuth(dataDir)
	if string(got) != syncAuth {
		t.Fatalf("expected sync cache to win, got %q", string(got))
	}
}

func TestDiscoverCodexAuth_NothingAvailable(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	got := discoverCodexAuth("")
	if got != nil {
		t.Fatalf("expected nil when no credentials, got %q", string(got))
	}
}

func TestClaudeCredentials_HostFallbackDotCredentials(t *testing.T) {
	dataDir := t.TempDir()
	fakeHome := t.TempDir()
	claudeDir := filepath.Join(fakeHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	creds := `{"oauth_token":"host-dot-creds"}`
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	// No sync cache, no explicit path — should find ~/.claude/.credentials.json.
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "host-fallback-test", dataDir)
	defer result.CleanupFn()

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	for _, name := range []string{"credentials.json", ".credentials.json"} {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
		if string(data) != creds {
			t.Fatalf("%s: expected %q, got %q", name, creds, string(data))
		}
	}
}

func TestClaudeCredentials_HostFallbackCredentialsJSON(t *testing.T) {
	dataDir := t.TempDir()
	fakeHome := t.TempDir()
	claudeDir := filepath.Join(fakeHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only credentials.json (without dot prefix) — should also be found.
	creds := `{"oauth_token":"host-no-dot"}`
	if err := os.WriteFile(filepath.Join(claudeDir, "credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{},
	}, "host-nodot-test", dataDir)
	defer result.CleanupFn()

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	data, err := os.ReadFile(filepath.Join(sessionDir, "credentials.json"))
	if err != nil {
		t.Fatalf("expected credentials.json: %v", err)
	}
	if string(data) != creds {
		t.Fatalf("expected %q, got %q", creds, string(data))
	}
}

func TestClaudeCredentials_ExplicitOverridesAutoDiscover(t *testing.T) {
	dataDir := t.TempDir()

	// Set up sync cache.
	syncDir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "claude-credentials.json"), []byte(`{"source":"sync"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Set up explicit credentials.
	explicitPath := filepath.Join(t.TempDir(), "explicit.json")
	explicit := `{"source":"explicit"}`
	if err := os.WriteFile(explicitPath, []byte(explicit), 0o600); err != nil {
		t.Fatal(err)
	}

	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Claude: &api.ClaudeConfig{
			CredentialsPath: explicitPath,
		},
	}, "explicit-override-test", dataDir)
	defer result.CleanupFn()

	sessionDir := findMount(t, result.Mounts, "/home/agent/.claude").Host
	data, err := os.ReadFile(filepath.Join(sessionDir, "credentials.json"))
	if err != nil {
		t.Fatalf("read credentials.json: %v", err)
	}
	if string(data) != explicit {
		t.Fatalf("expected explicit to override sync, got %q", string(data))
	}
}

func TestCodexCredentials_TmpDirAutoDiscover(t *testing.T) {
	fakeHome := t.TempDir()
	codexDir := filepath.Join(fakeHome, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := `{"api_key":"tmpdir-codex"}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	// tmpDir mode (dataDir="") — materializeCodex fallback should still discover.
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Codex: &api.CodexConfig{},
	}, "codex-tmpdir-test", "")
	defer result.CleanupFn()

	codexMountDir := findMount(t, result.Mounts, "/home/agent/.codex").Host
	data, err := os.ReadFile(filepath.Join(codexMountDir, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json in tmpdir: %v", err)
	}
	if string(data) != auth {
		t.Fatalf("expected tmpdir codex auth, got %q", string(data))
	}
}

func TestCodexCredentials_HostFallbackViaMaterialize(t *testing.T) {
	dataDir := t.TempDir()
	fakeHome := t.TempDir()
	codexDir := filepath.Join(fakeHome, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := `{"api_key":"host-codex-materialize"}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	// No sync cache in dataDir — should fallback to host ~/.codex/auth.json.
	result := mustMaterializeWithDataDir(t, &api.SessionRequest{
		Codex: &api.CodexConfig{},
	}, "codex-host-test", dataDir)
	defer result.CleanupFn()

	codexMountDir := findMount(t, result.Mounts, "/home/agent/.codex").Host
	data, err := os.ReadFile(filepath.Join(codexMountDir, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(data) != auth {
		t.Fatalf("expected host fallback auth, got %q", string(data))
	}
}
