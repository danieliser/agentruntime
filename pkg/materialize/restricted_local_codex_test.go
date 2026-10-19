package materialize

import (
	"os"
	"path/filepath"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/pelletier/go-toml/v2"
)

func TestRestrictedLocalReadScopeAndTemporaryPaths(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "repo", "my.app", ".claude", "worktrees", "worker")
	home := filepath.Join(root, "session", "home")
	for _, dir := range []string{work, home, filepath.Join(root, "repo", ".git")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	doc := filepath.Join(root, "repo", "AGENTS.md")
	if err := os.WriteFile(doc, []byte("repository instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	outsideDoc := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(outsideDoc, []byte("outside repository"), 0600); err != nil {
		t.Fatal(err)
	}
	doc, err := filepath.EvalSymlinks(doc)
	if err != nil {
		t.Fatal(err)
	}
	outsideDoc, err = filepath.EvalSymlinks(outsideDoc)
	if err != nil {
		t.Fatal(err)
	}
	req := &apischema.SessionRequest{WorkDir: work, ProviderPermissions: &apischema.ProviderPermissions{ApprovalPolicy: "on-request"}}
	data, err := RestrictedLocalCodexConfig(req, home, filepath.Join(home, ".codex"), filepath.Dir(home))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Permissions map[string]struct {
			Filesystem map[string]string `toml:"filesystem"`
		} `toml:"permissions"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	roots := config.Permissions[apischema.RestrictedLocalCodexProfile].Filesystem
	for _, path := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		if roots[path] != "deny" {
			t.Errorf("temporary root %s is not denied", path)
		}
	}
	if roots[doc] != "read" {
		t.Error("ancestor project instructions not readable")
	}
	if roots[outsideDoc] != "" {
		t.Error("instructions above repository root exposed")
	}
	if roots[filepath.Dir(doc)] != "" {
		t.Error("entire ancestor directory exposed")
	}
	req.WorkDir = filepath.Dir(doc)
	data, err = RestrictedLocalCodexConfig(req, home, filepath.Join(home, ".codex"), filepath.Dir(home))
	if err != nil {
		t.Fatal(err)
	}
	config.Permissions = nil
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Permissions[apischema.RestrictedLocalCodexProfile].Filesystem[outsideDoc] != "" {
		t.Error("repo-root work_dir exposes instructions above the repo")
	}

}

func TestRestrictedLocalInstructionScopeStopsAtRepositoryAndHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "owner")
	work := filepath.Join(home, "repo")
	for _, dir := range []string{work, filepath.Join(work, ".git")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	doc := filepath.Join(home, "AGENTS.md")
	if err := os.WriteFile(doc, []byte("owner instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	req := &apischema.SessionRequest{WorkDir: work, ProviderPermissions: &apischema.ProviderPermissions{ApprovalPolicy: "on-request"}}
	data, err := RestrictedLocalCodexConfig(req, filepath.Join(root, "session"), filepath.Join(root, "session", ".codex"), root)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Permissions map[string]struct {
			Filesystem map[string]string `toml:"filesystem"`
		} `toml:"permissions"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Permissions[apischema.RestrictedLocalCodexProfile].Filesystem[doc] != "" {
		t.Error("owner-home instructions exposed")
	}
}
