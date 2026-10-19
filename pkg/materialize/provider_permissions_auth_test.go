package materialize

import (
	api "github.com/danieliser/agentruntime/pkg/api/schema"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanCodingProviderPermissionsCopyOnlyTrustedProviderAuth(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, runtimeName := range []string{"docker", "local"} {
			t.Run(provider+"/"+runtimeName, func(t *testing.T) {
				dataDir := t.TempDir()
				home := t.TempDir()
				workspace := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("DATABASE_URL", "ambient-database-sentinel")
				t.Setenv("SLACK_BOT_TOKEN", "ambient-mail-sentinel")
				write := func(path, content string) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
				write(filepath.Join(workspace, ".claude", "settings.json"), `{"permissions":{"defaultMode":"bypassPermissions"},"sentinel":"ambient-settings-sentinel"}`)
				write(filepath.Join(workspace, ".mcp.json"), `{"mcpServers":{"ambient":{"url":"https://ambient-mcp-sentinel.invalid"}}}`)
				write(filepath.Join(workspace, ".codex", "config.toml"), "sandbox_mode = \"danger-full-access\"\n# ambient-settings-sentinel\n")
				write(filepath.Join(home, ".claude", "settings.json"), `{"sentinel":"ambient-settings-sentinel"}`)
				write(filepath.Join(home, ".codex", "config.toml"), "# ambient-settings-sentinel\n")
				auth := `{"synthetic_provider_login":"trusted-login-fixture"}`
				cacheName := "claude-credentials.json"
				if provider == "codex" {
					cacheName = "codex-auth.json"
				}
				write(filepath.Join(dataDir, "credentials", cacheName), auth)
				request := &api.SessionRequest{Agent: provider, Runtime: runtimeName, WorkDir: workspace, Context: "clean", AutoDiscover: false,
					ProviderPermissions: &api.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
				result := mustMaterializeWithDataDir(t, request, "clean-coding-"+provider, dataDir)
				defer result.CleanupFn()
				providerDir := findMount(t, result.Mounts, "/home/agent/."+provider).Host
				authName := ".credentials.json"
				if provider == "codex" {
					authName = "auth.json"
				}
				copied, err := os.ReadFile(filepath.Join(providerDir, authName))
				if err != nil {
					t.Fatal(err)
				}
				if string(copied) != auth {
					t.Fatal("trusted auth was not copied exactly")
				}
				info, err := os.Stat(filepath.Join(providerDir, authName))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm()&0077 != 0 {
					t.Fatal("copied auth permissions are not private")
				}
				if len(request.Env) != 0 {
					t.Fatal("ambient environment entered the materialized request")
				}
				err = filepath.Walk(providerDir, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if info.IsDir() {
						return nil
					}
					contents, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					for _, sentinel := range []string{"ambient-settings-sentinel", "ambient-mcp-sentinel", "ambient-database-sentinel", "ambient-mail-sentinel"} {
						if strings.Contains(string(contents), sentinel) {
							t.Fatal("ambient configuration or environment was copied")
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
