package materialize

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// File size limits
	maxFileSize      = 5 * 1024 * 1024  // 5 MB per file
	maxAggregateSzie = 20 * 1024 * 1024 // 20 MB total

	// Import depth limit
	maxImportDepth = 5
)

// DiscoverOptions controls which configuration categories are discovered.
type DiscoverOptions struct {
	ClaudeMD   bool `json:"claude_md"`
	Settings   bool `json:"settings"`
	MCP        bool `json:"mcp"`
	Rules      bool `json:"rules"`
	Agents     bool `json:"agents"`
	AgentsMD   bool `json:"agents_md"`  // Codex
	ConfigTOML bool `json:"config_toml"` // Codex
}

// ParseAutoDiscover converts an interface{} (bool, map, or nil) into a DiscoverOptions struct.
// Form 1: bool -> apply to all categories
// Form 2: map[string]bool -> granular control (unspecified keys default to false)
// Form 3: nil -> return nil (caller handles platform default)
func ParseAutoDiscover(raw interface{}) *DiscoverOptions {
	opts := &DiscoverOptions{}

	switch v := raw.(type) {
	case nil:
		// Platform default (caller decides)
		return nil

	case bool:
		// Shorthand: apply to all categories
		opts.ClaudeMD = v
		opts.Settings = v
		opts.MCP = v
		opts.Rules = v
		opts.Agents = v
		opts.AgentsMD = v
		opts.ConfigTOML = v
		return opts

	case map[string]interface{}:
		// Granular: all unspecified default to false (whitelist model)
		if val, ok := v["claude_md"].(bool); ok {
			opts.ClaudeMD = val
		}
		if val, ok := v["settings"].(bool); ok {
			opts.Settings = val
		}
		if val, ok := v["mcp"].(bool); ok {
			opts.MCP = val
		}
		if val, ok := v["rules"].(bool); ok {
			opts.Rules = val
		}
		if val, ok := v["agents"].(bool); ok {
			opts.Agents = val
		}
		if val, ok := v["agents_md"].(bool); ok {
			opts.AgentsMD = val
		}
		if val, ok := v["config_toml"].(bool); ok {
			opts.ConfigTOML = val
		}
		return opts

	default:
		// Invalid type: treat as nil (platform default)
		return nil
	}
}

// DiscoveredFile represents a file discovered during discovery.
type DiscoveredFile struct {
	Path    string // Absolute path on host
	Content string // Full file contents
	Source  string // "project" | "user" | "system" | "profile"
	Level   int    // Directory level (0 = work_dir, up for Claude; root for Codex)
}

// ClaudeDiscovery holds discovered Claude configuration files.
type ClaudeDiscovery struct {
	ClaudeMDFiles []DiscoveredFile // All found CLAUDE.md files (path, content, source)
	SettingsJSON  map[string]any   // Merged settings cascade
	McpJSON       map[string]any   // Merged .mcp.json
	RulesFiles    []DiscoveredFile // All .claude/rules/**/*.md files
	AgentsDir     string           // Path to .claude/agents/ if exists
	ProjectRoot   string           // Detected project root (.git/.hg/.sl)
}

// CodexDiscovery holds discovered Codex configuration files.
type CodexDiscovery struct {
	AgentsMD       string           // Concatenated AGENTS.md root-to-work_dir
	ConfigTOML     map[string]any   // Merged config.toml cascade
	InstructionsMD string           // instructions.md if exists
	ProjectRoot    string           // Detected project root
	TrustLevel     string           // "trusted" or "untrusted"
}

// DiscoverClaudeFiles returns discovered Claude configuration files.
// Discovers CLAUDE.md, settings.json cascade, .mcp.json, rules, and agents.
func DiscoverClaudeFiles(workDir string, opts DiscoverOptions) (*ClaudeDiscovery, error) {
	disc := &ClaudeDiscovery{
		ClaudeMDFiles: []DiscoveredFile{},
		SettingsJSON:  map[string]any{},
		McpJSON:       map[string]any{},
		RulesFiles:    []DiscoveredFile{},
	}

	if workDir == "" {
		return disc, nil
	}

	resolved, err := resolvePath(workDir)
	if err != nil {
		return nil, err
	}

	aggregate := 0

	// Discover CLAUDE.md files (walk up from workDir to root)
	if opts.ClaudeMD {
		claudeFiles, size, err := discoverClaudeMDFiles(resolved)
		if err != nil {
			return nil, err
		}
		disc.ClaudeMDFiles = claudeFiles
		aggregate += size
	}

	// Discover settings.json cascade (system -> user -> project)
	if opts.Settings {
		settingsMap, size, err := discoverSettingsJSON(resolved)
		if err != nil {
			return nil, err
		}
		disc.SettingsJSON = settingsMap
		aggregate += size
	}

	// Discover .mcp.json (project root + user level)
	if opts.MCP {
		mcpMap, size, err := discoverMcpJSON(resolved)
		if err != nil {
			return nil, err
		}
		disc.McpJSON = mcpMap
		aggregate += size
	}

	// Discover .claude/rules/ (user + project)
	if opts.Rules {
		rulesFiles, size, err := discoverRulesFiles(resolved)
		if err != nil {
			return nil, err
		}
		disc.RulesFiles = rulesFiles
		aggregate += size
	}

	// Discover .claude/agents/ directory
	if opts.Agents {
		agentsDir := discoverAgentsDir(resolved)
		disc.AgentsDir = agentsDir
	}

	// Detect project root for reference
	disc.ProjectRoot = findProjectRoot(resolved)

	if aggregate > maxAggregateSzie {
		return nil, fmt.Errorf("discovered files exceed aggregate size limit (20 MB)")
	}

	return disc, nil
}

// DiscoverCodexFiles returns discovered Codex configuration files.
// Discovers AGENTS.md, config.toml cascade, project root, and instructions.
func DiscoverCodexFiles(workDir string, opts DiscoverOptions) (*CodexDiscovery, error) {
	disc := &CodexDiscovery{
		AgentsMD:   "",
		ConfigTOML: map[string]any{},
		TrustLevel: "untrusted",
	}

	if workDir == "" {
		return disc, nil
	}

	resolved, err := resolvePath(workDir)
	if err != nil {
		return nil, err
	}

	// Find project root
	projectRoot := findProjectRoot(resolved)
	disc.ProjectRoot = projectRoot

	aggregate := 0

	// Discover AGENTS.md files (walk down from project root to workDir)
	if opts.AgentsMD {
		agentsMD, size, err := discoverAgentsMDFiles(projectRoot, resolved)
		if err != nil {
			return nil, err
		}
		disc.AgentsMD = agentsMD
		aggregate += size
	}

	// Discover config.toml cascade (system -> user -> project)
	if opts.ConfigTOML {
		configMap, size, err := discoverConfigTOML(resolved, projectRoot)
		if err != nil {
			return nil, err
		}
		disc.ConfigTOML = configMap
		aggregate += size
	}

	if aggregate > maxAggregateSzie {
		return nil, fmt.Errorf("discovered files exceed aggregate size limit (20 MB)")
	}

	return disc, nil
}

// discoverClaudeMDFiles walks up from workDir to root, collecting all CLAUDE.md files.
func discoverClaudeMDFiles(workDir string) ([]DiscoveredFile, int, error) {
	var files []DiscoveredFile
	aggregate := 0
	current := workDir

	for {
		// Check for CLAUDE.md at this level
		claudemdPath := filepath.Join(current, "CLAUDE.md")
		if content, err := readFileLimited(claudemdPath, maxFileSize); err == nil {
			aggregate += len(content)
			if aggregate > maxAggregateSzie {
				return files, aggregate, fmt.Errorf("aggregate size exceeded during CLAUDE.md discovery")
			}
			files = append(files, DiscoveredFile{
				Path:    claudemdPath,
				Content: content,
				Source:  "project",
				Level:   0,
			})
		}

		// Check for .claude/CLAUDE.md at this level
		claudeSubdirPath := filepath.Join(current, ".claude", "CLAUDE.md")
		if content, err := readFileLimited(claudeSubdirPath, maxFileSize); err == nil {
			aggregate += len(content)
			if aggregate > maxAggregateSzie {
				return files, aggregate, fmt.Errorf("aggregate size exceeded during CLAUDE.md discovery")
			}
			files = append(files, DiscoveredFile{
				Path:    claudeSubdirPath,
				Content: content,
				Source:  "project",
				Level:   0,
			})
		}

		// Walk up
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root
			break
		}
		current = parent
	}

	// Add user-level CLAUDE.md (always loaded)
	home, err := os.UserHomeDir()
	if err == nil {
		userClaudemdPath := filepath.Join(home, ".claude", "CLAUDE.md")
		if content, err := readFileLimited(userClaudemdPath, maxFileSize); err == nil {
			aggregate += len(content)
			if aggregate > maxAggregateSzie {
				return files, aggregate, fmt.Errorf("aggregate size exceeded during user CLAUDE.md discovery")
			}
			files = append(files, DiscoveredFile{
				Path:    userClaudemdPath,
				Content: content,
				Source:  "user",
				Level:   0,
			})
		}
	}

	return files, aggregate, nil
}

// discoverSettingsJSON returns merged settings from cascade: system -> user -> project.
func discoverSettingsJSON(workDir string) (map[string]any, int, error) {
	merged := map[string]any{}
	aggregate := 0

	// For now, we skip system-level settings (platform-specific paths)
	// and focus on user and project levels

	// User level: ~/.claude/settings.json
	home, err := os.UserHomeDir()
	if err == nil {
		userSettingsPath := filepath.Join(home, ".claude", "settings.json")
		if content, err := readFileLimited(userSettingsPath, maxFileSize); err == nil {
			var userSettings map[string]any
			if err := json.Unmarshal([]byte(content), &userSettings); err == nil {
				aggregate += len(content)
				merged = deepMerge(merged, userSettings)
			}
		}
	}

	// Project level: .claude/settings.json
	projectSettingsPath := filepath.Join(workDir, ".claude", "settings.json")
	if content, err := readFileLimited(projectSettingsPath, maxFileSize); err == nil {
		var projectSettings map[string]any
		if err := json.Unmarshal([]byte(content), &projectSettings); err == nil {
			aggregate += len(content)
			merged = deepMerge(merged, projectSettings)
		}
	}

	// Project-local (gitignored): .claude/settings.local.json
	projectLocalPath := filepath.Join(workDir, ".claude", "settings.local.json")
	if content, err := readFileLimited(projectLocalPath, maxFileSize); err == nil {
		var localSettings map[string]any
		if err := json.Unmarshal([]byte(content), &localSettings); err == nil {
			aggregate += len(content)
			merged = deepMerge(merged, localSettings)
		}
	}

	if aggregate > maxAggregateSzie {
		return merged, aggregate, fmt.Errorf("aggregate size exceeded during settings discovery")
	}

	return merged, aggregate, nil
}

// discoverMcpJSON returns merged MCP configuration from project and user levels.
func discoverMcpJSON(workDir string) (map[string]any, int, error) {
	merged := map[string]any{}
	aggregate := 0

	// User level: ~/.claude.json (contains mcpServers key)
	home, err := os.UserHomeDir()
	if err == nil {
		userMcpPath := filepath.Join(home, ".claude.json")
		if content, err := readFileLimited(userMcpPath, maxFileSize); err == nil {
			var userConfig map[string]any
			if err := json.Unmarshal([]byte(content), &userConfig); err == nil {
				aggregate += len(content)
				merged = deepMerge(merged, userConfig)
			}
		}
	}

	// Project level: .mcp.json at project root
	projectRoot := findProjectRoot(workDir)
	if projectRoot != "" {
		projectMcpPath := filepath.Join(projectRoot, ".mcp.json")
		if content, err := readFileLimited(projectMcpPath, maxFileSize); err == nil {
			var projectMcp map[string]any
			if err := json.Unmarshal([]byte(content), &projectMcp); err == nil {
				aggregate += len(content)
				merged = deepMerge(merged, projectMcp)
			}
		}
	}

	if aggregate > maxAggregateSzie {
		return merged, aggregate, fmt.Errorf("aggregate size exceeded during MCP discovery")
	}

	return merged, aggregate, nil
}

// discoverRulesFiles discovers all .claude/rules/**/*.md files.
func discoverRulesFiles(workDir string) ([]DiscoveredFile, int, error) {
	var files []DiscoveredFile
	aggregate := 0

	projectRoot := findProjectRoot(workDir)

	// Project rules: .claude/rules/
	if projectRoot != "" {
		projectRulesDir := filepath.Join(projectRoot, ".claude", "rules")
		if rulesFiles, size, err := walkRulesDir(projectRulesDir, "project"); err == nil {
			files = append(files, rulesFiles...)
			aggregate += size
		}
	}

	// User rules: ~/.claude/rules/
	home, err := os.UserHomeDir()
	if err == nil {
		userRulesDir := filepath.Join(home, ".claude", "rules")
		if rulesFiles, size, err := walkRulesDir(userRulesDir, "user"); err == nil {
			files = append(files, rulesFiles...)
			aggregate += size
		}
	}

	if aggregate > maxAggregateSzie {
		return files, aggregate, fmt.Errorf("aggregate size exceeded during rules discovery")
	}

	return files, aggregate, nil
}

// walkRulesDir recursively walks a rules directory.
func walkRulesDir(dir string, source string) ([]DiscoveredFile, int, error) {
	var files []DiscoveredFile
	aggregate := 0

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Skip inaccessible paths
		}

		if d.IsDir() {
			return nil
		}

		if strings.HasSuffix(d.Name(), ".md") {
			content, err := readFileLimited(path, maxFileSize)
			if err != nil {
				return nil // Skip oversized files
			}
			aggregate += len(content)
			if aggregate > maxAggregateSzie {
				return fmt.Errorf("size limit exceeded")
			}
			files = append(files, DiscoveredFile{
				Path:    path,
				Content: content,
				Source:  source,
				Level:   0,
			})
		}
		return nil
	})

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return files, aggregate, err
	}

	return files, aggregate, nil
}

// discoverAgentsDir returns the path to .claude/agents/ if it exists.
func discoverAgentsDir(workDir string) string {
	projectRoot := findProjectRoot(workDir)
	if projectRoot != "" {
		agentsDir := filepath.Join(projectRoot, ".claude", "agents")
		if info, err := os.Stat(agentsDir); err == nil && info.IsDir() {
			return agentsDir
		}
	}
	return ""
}

// discoverAgentsMDFiles walks down from project root to workDir, concatenating AGENTS.md files.
func discoverAgentsMDFiles(projectRoot, workDir string) (string, int, error) {
	if projectRoot == "" {
		return "", 0, nil
	}

	var parts []string
	aggregate := 0

	// Build path list from root to workDir
	paths := buildPathList(projectRoot, workDir)

	// Walk down and collect files
	for _, path := range paths {
		// Check for AGENTS.override.md first
		overridePath := filepath.Join(path, "AGENTS.override.md")
		if content, err := readFileLimited(overridePath, maxFileSize); err == nil {
			aggregate += len(content)
			if aggregate > maxAggregateSzie {
				return "", aggregate, fmt.Errorf("aggregate size exceeded during AGENTS.md discovery")
			}
			parts = append(parts, content)
			continue
		}

		// Fall back to AGENTS.md
		agentsPath := filepath.Join(path, "AGENTS.md")
		if content, err := readFileLimited(agentsPath, maxFileSize); err == nil {
			aggregate += len(content)
			if aggregate > maxAggregateSzie {
				return "", aggregate, fmt.Errorf("aggregate size exceeded during AGENTS.md discovery")
			}
			parts = append(parts, content)
			continue
		}

		// Fallback chain: TEAM_GUIDE.md, .agents.md
		for _, fallback := range []string{"TEAM_GUIDE.md", ".agents.md"} {
			fallbackPath := filepath.Join(path, fallback)
			if content, err := readFileLimited(fallbackPath, maxFileSize); err == nil {
				aggregate += len(content)
				if aggregate > maxAggregateSzie {
					return "", aggregate, fmt.Errorf("aggregate size exceeded during AGENTS.md discovery")
				}
				parts = append(parts, content)
				break
			}
		}
	}

	// Add user-level AGENTS.md (append last)
	home, err := os.UserHomeDir()
	if err == nil {
		userOverridePath := filepath.Join(home, ".codex", "AGENTS.override.md")
		if content, err := readFileLimited(userOverridePath, maxFileSize); err == nil {
			aggregate += len(content)
			if aggregate > maxAggregateSzie {
				return "", aggregate, fmt.Errorf("aggregate size exceeded during user AGENTS.md discovery")
			}
			parts = append(parts, content)
		} else {
			userAgentsPath := filepath.Join(home, ".codex", "AGENTS.md")
			if content, err := readFileLimited(userAgentsPath, maxFileSize); err == nil {
				aggregate += len(content)
				if aggregate > maxAggregateSzie {
					return "", aggregate, fmt.Errorf("aggregate size exceeded during user AGENTS.md discovery")
				}
				parts = append(parts, content)
			}
		}
	}

	// Concatenate with blank lines
	result := strings.Join(parts, "\n\n")
	return result, aggregate, nil
}

// discoverConfigTOML returns merged config.toml from cascade.
func discoverConfigTOML(workDir string, projectRoot string) (map[string]any, int, error) {
	merged := map[string]any{}
	aggregate := 0

	// User level: ~/.codex/config.toml
	home, err := os.UserHomeDir()
	if err == nil {
		userConfigPath := filepath.Join(home, ".codex", "config.toml")
		if content, err := readFileLimited(userConfigPath, maxFileSize); err == nil {
			aggregate += len(content)
			if configMap, err := parseTOMLSimple(content); err == nil {
				merged = deepMerge(merged, configMap)
			}
		}
	}

	// Project level: .codex/config.toml (if trusted)
	if projectRoot != "" {
		projectConfigPath := filepath.Join(projectRoot, ".codex", "config.toml")
		if content, err := readFileLimited(projectConfigPath, maxFileSize); err == nil {
			aggregate += len(content)
			if configMap, err := parseTOMLSimple(content); err == nil {
				merged = deepMerge(merged, configMap)
			}
		}
	}

	if aggregate > maxAggregateSzie {
		return merged, aggregate, fmt.Errorf("aggregate size exceeded during config.toml discovery")
	}

	return merged, aggregate, nil
}

// Utility functions

// resolvePath resolves symlinks and verifies the path is absolute.
func resolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}

	cleaned := filepath.Clean(resolved)
	if !filepath.IsAbs(cleaned) {
		return "", errors.New("path must be absolute")
	}

	return cleaned, nil
}

// readFileLimited reads a file with size limit enforcement.
func readFileLimited(path string, maxSize int) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	if info.Size() > int64(maxSize) {
		return "", fmt.Errorf("file too large: %d > %d", info.Size(), maxSize)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	return string(content), nil
}

// findProjectRoot walks up to find .git, .hg, .sl, or returns empty string.
func findProjectRoot(startDir string) string {
	current := startDir
	for {
		for _, marker := range []string{".git", ".hg", ".sl"} {
			markerPath := filepath.Join(current, marker)
			if _, err := os.Stat(markerPath); err == nil {
				return current
			}
		}

		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// buildPathList builds a slice of paths from root to workDir (inclusive).
func buildPathList(root, workDir string) []string {
	var paths []string
	current := workDir

	for {
		paths = append([]string{current}, paths...) // Prepend to build root-first order
		if current == root {
			break
		}

		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	return paths
}

// deepMerge recursively merges right into left, right winning on conflicts.
func deepMerge(left, right map[string]any) map[string]any {
	if left == nil {
		left = map[string]any{}
	}

	for key, rval := range right {
		lval := left[key]

		switch rv := rval.(type) {
		case map[string]any:
			if lm, ok := lval.(map[string]any); ok {
				left[key] = deepMerge(lm, rv)
			} else {
				left[key] = cloneMap(rv)
			}
		case []any:
			if lexists, ok := lval.([]any); ok {
				// Merge arrays with dedup
				merged := lexists
				seen := make(map[string]bool)
				for _, item := range lexists {
					key := fmt.Sprintf("%v", item)
					seen[key] = true
				}
				for _, item := range rv {
					key := fmt.Sprintf("%v", item)
					if !seen[key] {
						merged = append(merged, item)
						seen[key] = true
					}
				}
				left[key] = merged
			} else {
				left[key] = cloneSlice(rv)
			}
		default:
			left[key] = rval
		}
	}

	return left
}

func cloneMap(m map[string]any) map[string]any {
	cloned := make(map[string]any, len(m))
	for k, v := range m {
		cloned[k] = v
	}
	return cloned
}

func cloneSlice(s []any) []any {
	cloned := make([]any, len(s))
	copy(cloned, s)
	return cloned
}

// parseTOMLSimple does a naive TOML parse for flat key=value pairs.
// This is a simplified parser for basic TOML files with top-level keys only.
func parseTOMLSimple(content string) (map[string]any, error) {
	result := map[string]any{}

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Skip section headers for now
		if strings.HasPrefix(line, "[") {
			continue
		}

		// Parse key=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Simple value parsing (strings only for now)
		if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
			result[key] = value[1 : len(value)-1]
		} else if value == "true" {
			result[key] = true
		} else if value == "false" {
			result[key] = false
		} else {
			result[key] = value
		}
	}

	return result, nil
}

// mergeClaudeMD concatenates discovered CLAUDE.md files in order.
// Discovery order is: system -> user -> project (root upward to work_dir).
func mergeClaudeMD(files []DiscoveredFile) string {
	var parts []string
	for _, f := range files {
		if f.Content != "" {
			parts = append(parts, f.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}
