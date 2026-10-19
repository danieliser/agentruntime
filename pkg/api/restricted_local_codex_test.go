package api

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRestrictedLocalCodexDisablesAncestorAndSymlinkConfig(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "my.app", ".claude", "worktrees", "project")
	if err := os.MkdirAll(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	args, err := restrictedLocalCodexArgs(alias)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	for _, start := range []string{alias, canonical} {
		for path := start; ; path = filepath.Dir(path) {
			if !strings.Contains(strings.Join(args, " "), strconv.Quote(path)+"={trust_level=\"untrusted\"}") {
				t.Fatalf("project layer at %q not disabled", path)
			}
			if filepath.Dir(path) == path {
				break
			}
		}
	}
	for _, setting := range []string{"allow_login_shell=false", "approvals_reviewer=\"user\"", "hooks", "plugins", "enable_mcp_apps", "multi_agent"} {
		if !slices.Contains(args, setting) {
			t.Errorf("missing isolation setting %s", setting)
		}
	}
}
