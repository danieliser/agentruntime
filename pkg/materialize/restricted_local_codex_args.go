package materialize

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RestrictedLocalCodexArgs prevents ambient project configuration from adding
// permissions, hooks or external tools. A single inline table preserves literal
// dots in path keys under Codex's strict CLI config parser.
func RestrictedLocalCodexArgs(workDir string) ([]string, error) {
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, root := range []string{workDir, resolved} {
		for path := filepath.Clean(root); !seen[path]; path = filepath.Dir(path) {
			seen[path] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	projects := make([]string, 0, len(paths))
	for _, path := range paths {
		projects = append(projects, strconv.Quote(path)+`={trust_level="untrusted"}`)
	}
	args := []string{"-c", "allow_login_shell=false", "-c", `approvals_reviewer="user"`, "-c", "projects={" + strings.Join(projects, ",") + "}"}
	for _, feature := range []string{"hooks", "plugins", "enable_mcp_apps", "multi_agent", "js_repl", "browser_use", "computer_use", "image_generation"} {
		args = append(args, "--disable", feature)
	}
	return args, nil
}
