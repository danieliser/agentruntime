package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestRestrictedLocalClaudeMaterializationAndCleanup(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "agentd")
	work := filepath.Join(root, "project")
	for _, dir := range []string{filepath.Join(data, "credentials"), work} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	master := filepath.Join(data, "credentials", "claude-credentials.json")
	if err := os.WriteFile(master, []byte(`{"synthetic_provider_login":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(data, "credentials", "other.json"), filepath.Join(data, "settings.json")} {
		if err := os.WriteFile(path, []byte("global-only-sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NODE_OPTIONS", "ambient-injection")
	t.Setenv("CLAUDE_CONFIG_DIR", "ambient-config")
	t.Setenv("CLAUDE_CODE_FUTURE_SESSION_THING", "ambient-session")
	t.Setenv("DATABASE_URL", "ambient-secret")
	req := &apischema.SessionRequest{Agent: "claude", Runtime: "local", WorkDir: work, ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
	command, env, cleanup, err := materializeRestrictedLocal(SpawnConfig{SessionID: "fixture", DataDir: data, WorkDir: work, Cmd: []string{"claude"}, Request: req})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(command) != 3 || command[1] != "--settings" {
		t.Fatal("missing generated settings")
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
		if strings.Contains(value, "ambient-") {
			t.Fatal("ambient value inherited")
		}
	}
	state := filepath.Join(data, "claude-sessions", "fixture")
	home := filepath.Join(state, "local-home")
	config := filepath.Join(home, ".claude")
	if values["HOME"] != home || values["CLAUDE_CONFIG_DIR"] != config || values["CODEX_HOME"] != "" {
		t.Fatal("wrong provider home")
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		if !strings.HasPrefix(values[key], home+string(os.PathSeparator)) {
			t.Fatal("unconfined home/temp", key)
		}
	}
	for _, name := range []string{".credentials.json", "credentials.json", "restricted-settings.json"} {
		info, err := os.Stat(filepath.Join(config, name))
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal("missing/private credential or config", name, err)
		}
	}
	for _, name := range []string{".credentials.json", "credentials.json"} {
		data, err := os.ReadFile(filepath.Join(config, name))
		if err != nil || string(data) != `{"synthetic_provider_login":"fixture"}` {
			t.Fatal("provider auth cache not copied exactly", name, err)
		}
	}
	if err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "global-only-sentinel") {
				t.Error("unrelated daemon data copied", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"projects", "sessions"} {
		path, err := filepath.EvalSymlinks(filepath.Join(config, name))
		if err != nil {
			t.Fatal("conversation state not durable", name, err)
		}
		// macOS temporary roots may use /var while their real path is /private/var.
		want, err := filepath.EvalSymlinks(filepath.Join(state, name))
		if err != nil || path != want {
			t.Fatal("conversation state not durable", name, err)
		}
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("launch home retained")
	}
	for _, name := range []string{".credentials.json", "credentials.json"} {
		if _, err := os.Stat(filepath.Join(state, name)); !os.IsNotExist(err) {
			t.Fatal("copied auth retained")
		}
	}
	if _, err := os.Stat(master); err != nil {
		t.Fatal("master changed", err)
	}
	if _, err := os.Stat(filepath.Join(state, "projects")); err != nil {
		t.Fatal("conversation removed", err)
	}
}

func TestRestrictedLocalReusesInterruptedHome(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			req := &apischema.SessionRequest{Agent: provider, Runtime: "local", WorkDir: work, ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "never"}}
			cfg := SpawnConfig{SessionID: "fixture", DataDir: filepath.Join(root, "data"), WorkDir: work, Cmd: []string{provider}, Request: req}
			_, _, cleanup, err := materializeRestrictedLocal(cfg)
			if cleanup != nil {
				t.Cleanup(func() {
					if err := cleanup(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(cfg.DataDir, provider+"-sessions", "fixture")
			stale := filepath.Join(state, "local-home", "orphan-credential")
			keep := filepath.Join(state, "sessions", "conversation.json")
			if err := os.WriteFile(stale, []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keep, []byte("durable"), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = materializeRestrictedLocal(cfg)
			if err != nil {
				t.Fatal("interrupted home cannot be reused", err)
			}
			if _, err := os.Stat(stale); !os.IsNotExist(err) {
				t.Fatal("stale home retained", err)
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "durable" {
				t.Fatal("conversation changed", err)
			}
		})
	}
}
