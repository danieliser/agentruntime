package materialize

import (
	"os"
	"os/exec"
	"path/filepath"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

// RestrictedLocalCodexConfig builds the same named profile used by native
// threads and the no-model sandbox probes. No legacy sandbox_mode is emitted:
// it would override default_permissions and restore unrestricted host reads.
func RestrictedLocalCodexConfig(req *apischema.SessionRequest, home, providerHome, stateDir string) ([]byte, error) {
	roots := map[string]any{":minimal": "read", req.WorkDir: "write", home: "write", "/opt/homebrew": "read"}
	for _, path := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		roots[path] = "deny"
	}
	for _, path := range restrictedLocalInstructionFiles(req.WorkDir) {
		roots[path] = "read"
	}
	// The host provider can authenticate and maintain its state outside its
	// command sandbox. Tool processes cannot read or modify provider auth/config.
	roots[providerHome] = "deny"
	if binary, err := exec.LookPath("codex"); err == nil {
		if physical, err := filepath.EvalSymlinks(binary); err == nil {
			roots[physical] = "read"
		}
	}
	excluded := append([]string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}, req.SecretGrants...)

	config := map[string]any{
		"default_permissions":      apischema.RestrictedLocalCodexProfile,
		"approval_policy":          req.ProviderPermissions.ApprovalPolicy,
		"approvals_reviewer":       "user",
		"allow_login_shell":        false,
		"shell_environment_policy": map[string]any{"inherit": "core", "exclude": excluded},
		"sqlite_home":              stateDir,
		"permissions": map[string]any{apischema.RestrictedLocalCodexProfile: map[string]any{
			"filesystem": roots, "network": map[string]any{"enabled": false},
		}},
		"features":   map[string]any{"hooks": false, "plugins": false, "enable_mcp_apps": false, "multi_agent": false, "js_repl": false, "browser_use": false, "computer_use": false, "image_generation": false},
		"web_search": "disabled",
	}
	// Existing native workspace protections must survive the custom profile.
	for _, name := range []string{".git", ".agents", ".codex", ".aws"} {
		roots[filepath.Join(req.WorkDir, name)] = "read"
	}
	if req.Model != "" {
		config["model"] = req.Model
	}
	if req.Effort != "" {
		config["model_reasoning_effort"] = req.Effort
	}
	if req.Fast {
		config["service_tier"] = "priority"
	}
	return marshalSimpleTOML(config)
}

// Stop at the nearest Git root (including a linked-worktree .git file). Never
// grant an ancestor directory, owner-home instructions, or a symlink target.
func restrictedLocalInstructionFiles(work string) []string {
	resolved, err := filepath.EvalSymlinks(work)
	if err != nil {
		return nil
	}
	ownerHome, _ := os.UserHomeDir()
	if physical, err := filepath.EvalSymlinks(ownerHome); err == nil {
		ownerHome = physical
	}
	repo := ""
	for dir := resolved; dir != ownerHome && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			repo = dir
			break
		}
	}
	if repo == "" || repo == resolved {
		return nil
	}
	files := []string{}
	for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
		for _, name := range []string{"AGENTS.md", "AGENTS.override.md"} {
			path := filepath.Join(dir, name)
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
				files = append(files, path)
			}
		}
		if dir == repo {
			break
		}
	}
	return files
}
