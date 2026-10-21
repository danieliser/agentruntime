package runtime

import (
	"github.com/danieliser/agentruntime/internal/testutil"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestRestrictedLocalDoesNotInheritAmbientEnvironment(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("only macOS is qualified")
	}
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "codex"), []byte(testutil.CodexQualificationScript("exit 1")), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(fake, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	InitializeRestrictedLocalQualification(t.TempDir())
	awaitGlobalQualification(t)
	for _, provider := range []string{"codex"} {
		t.Run(provider, func(t *testing.T) {
			ambient := t.TempDir()
			t.Setenv("HOME", ambient)
			t.Setenv("CODEX_HOME", ambient)
			t.Setenv("CLAUDE_CONFIG_DIR", ambient)
			t.Setenv("NODE_OPTIONS", "ambient-config-sentinel")
			t.Setenv("CLAUDE_CODE_FUTURE_SESSION_THING", "ambient-session-sentinel")
			t.Setenv("DATABASE_URL", "ambient-secret-sentinel")
			dataDir := t.TempDir()
			sessionDir := filepath.Join(dataDir, provider+"-sessions", "local-isolation")
			req := &apischema.SessionRequest{Agent: provider, Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
			handle, err := NewLocalRuntime().Spawn(testContext(t), SpawnConfig{DataDir: dataDir, SessionID: "local-isolation", AgentName: provider, Cmd: []string{"/usr/bin/env"}, WorkDir: req.WorkDir, Request: req, SessionDir: &sessionDir})
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr, result := readProcessOutput(t, handle)
			if result.Code != 0 || result.Err != nil {
				t.Fatalf("exit=%+v stderr=%s", result, stderr)
			}
			for _, bad := range []string{ambient, "ambient-config-sentinel", "ambient-session-sentinel", "ambient-secret-sentinel"} {
				if strings.Contains(stdout, bad) {
					t.Fatal("ambient config leaked to subprocess")
				}
			}
			env := map[string]string{}
			for _, line := range strings.Split(stdout, "\n") {
				k, v, ok := strings.Cut(line, "=")
				if ok {
					env[k] = v
				}
			}
			for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "TMPDIR", "TMP", "TEMP"} {
				if !strings.HasPrefix(env[key], sessionDir+string(os.PathSeparator)) {
					t.Errorf("%s is not isolated", key)
				}
			}
			key := "CODEX_HOME"
			if provider == "claude" {
				key = "CLAUDE_CONFIG_DIR"
			}
			if env[key] != filepath.Join(sessionDir, "local-home", ".codex") {
				t.Errorf("%s not using durable session directory", key)
			}
			if _, err := os.Stat(filepath.Join(sessionDir, "local-home")); !os.IsNotExist(err) {
				t.Fatal("credential-bearing launch home not removed")
			}
			if _, err := os.Stat(sessionDir); err != nil {
				t.Fatal("session state not retained", err)
			}
		})
	}
}

func TestRestrictedLocalCleanupOnFailedLaunch(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("only macOS is qualified")
	}
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "codex"), []byte(testutil.CodexQualificationScript("exit 1")), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(fake, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	InitializeRestrictedLocalQualification(t.TempDir())
	awaitGlobalQualification(t)
	t.Setenv("HOME", t.TempDir())
	data := t.TempDir()
	cache := filepath.Join(data, "credentials")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(cache, "codex-auth.json")
	if err := os.WriteFile(master, []byte(`{"synthetic":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	req := &apischema.SessionRequest{Agent: "codex", Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "never"}}
	_, err := NewLocalRuntime().Spawn(testContext(t), SpawnConfig{DataDir: data, SessionID: "failure", WorkDir: req.WorkDir, Request: req, Cmd: []string{"/nonexistent/fixture-provider"}})
	if err == nil {
		t.Fatal("launch unexpectedly succeeded")
	}
	for _, name := range []string{"local-home", "auth.json"} {
		if _, err := os.Stat(filepath.Join(data, "codex-sessions", "failure", name)); !os.IsNotExist(err) {
			t.Error("credential artifact remains after failed launch")
		}
	}
	if _, err := os.Stat(master); err != nil {
		t.Fatal("master credential was changed")
	}
}

func TestRestrictedLocalWorkspaceCannotExposeStorageOrHostRoot(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "state")
	project := filepath.Join(root, "project")
	for _, dir := range []string{storage, project} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, work := range []string{root, storage, "/", "/usr"} {
		if validateRestrictedLocalPaths(work, storage) == nil {
			t.Fatal("unsafe overlap accepted")
		}
	}
	if err := validateRestrictedLocalPaths(project, storage); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictedLocalRemovesCopiedAuthOnProcessExit(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("only macOS is qualified")
	}
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "codex"), []byte(testutil.CodexQualificationScript("exit 1")), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(fake, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	InitializeRestrictedLocalQualification(t.TempDir())
	awaitGlobalQualification(t)
	t.Setenv("HOME", t.TempDir())
	data := t.TempDir()
	cache := filepath.Join(data, "credentials")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(cache, "codex-auth.json")
	if err := os.WriteFile(master, []byte(`{"synthetic":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	req := &apischema.SessionRequest{Agent: "codex", Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "never"}}
	handle, err := NewLocalRuntime().Spawn(testContext(t), SpawnConfig{DataDir: data, SessionID: "success", WorkDir: req.WorkDir, Request: req, Cmd: []string{"/bin/sh", "-c", `test -f "$CODEX_HOME/auth.json"`}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, result := readProcessOutput(t, handle)
	if result.Err != nil || result.Code != 0 {
		t.Fatal("copied provider auth absent before exit")
	}
	for _, name := range []string{"local-home", "auth.json"} {
		if _, err := os.Stat(filepath.Join(data, "codex-sessions", "success", name)); !os.IsNotExist(err) {
			t.Error("credential artifact remains after process exit")
		}
	}
	if _, err := os.Stat(master); err != nil {
		t.Fatal("master credential was changed")
	}
	if _, err := os.Stat(filepath.Join(data, "codex-sessions", "success", "sessions")); err != nil {
		t.Fatal("conversation state was removed")
	}
}
