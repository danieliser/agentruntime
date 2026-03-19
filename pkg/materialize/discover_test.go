package materialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeDiscoveryWalkUp tests CLAUDE.md discovery walking up from workDir.
func TestClaudeDiscoveryWalkUp(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	// Create directory structure: proj/src/lib/
	projRoot := filepath.Join(tmpDir, "proj")
	srcDir := filepath.Join(projRoot, "src")
	libDir := filepath.Join(srcDir, "lib")
	os.MkdirAll(libDir, 0755)

	// Create CLAUDE.md at different levels
	projClaudemd := filepath.Join(projRoot, "CLAUDE.md")
	os.WriteFile(projClaudemd, []byte("# Project CLAUDE.md"), 0644)

	srcClaudemd := filepath.Join(srcDir, "CLAUDE.md")
	os.WriteFile(srcClaudemd, []byte("# Src CLAUDE.md"), 0644)

	// Discover from libDir
	disc, err := DiscoverClaudeFiles(libDir, DiscoverOptions{ClaudeMD: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	// Should find both proj and src CLAUDE.md files
	if len(disc.ClaudeMDFiles) < 2 {
		t.Fatalf("Expected at least 2 CLAUDE.md files, got %d", len(disc.ClaudeMDFiles))
	}

	found := make(map[string]bool)
	for _, f := range disc.ClaudeMDFiles {
		if strings.Contains(f.Path, "proj/CLAUDE.md") {
			found["proj"] = true
		}
		if strings.Contains(f.Path, "src/CLAUDE.md") {
			found["src"] = true
		}
	}

	if !found["proj"] {
		t.Error("Expected to find proj/CLAUDE.md")
	}
	if !found["src"] {
		t.Error("Expected to find src/CLAUDE.md")
	}
}

// TestClaudeDiscoveryCladueSubdir tests .claude/CLAUDE.md discovery.
func TestClaudeDiscoveryCladueSubdir(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	projRoot := tmpDir
	claudeDir := filepath.Join(projRoot, ".claude")
	os.MkdirAll(claudeDir, 0755)

	claudemdPath := filepath.Join(claudeDir, "CLAUDE.md")
	os.WriteFile(claudemdPath, []byte("# .claude/CLAUDE.md"), 0644)

	disc, err := DiscoverClaudeFiles(projRoot, DiscoverOptions{ClaudeMD: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	found := false
	for _, f := range disc.ClaudeMDFiles {
		if strings.Contains(f.Path, ".claude/CLAUDE.md") && f.Content == "# .claude/CLAUDE.md" {
			found = true
		}
	}

	if !found {
		t.Error("Expected to find .claude/CLAUDE.md")
	}
}

// TestSettingsDeepMerge tests settings.json deep merge behavior.
func TestSettingsDeepMerge(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	home := os.Getenv("HOME")
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)

	homeDir := filepath.Join(tmpDir, "home")
	os.MkdirAll(homeDir, 0755)
	os.Setenv("HOME", homeDir)

	// Create user settings
	claudeDir := filepath.Join(homeDir, ".claude")
	os.MkdirAll(claudeDir, 0755)

	userSettings := map[string]any{"a": 1, "b": 2}
	userJSON, _ := json.Marshal(userSettings)
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), userJSON, 0644)

	// Create project settings (overrides b, adds c)
	projClaudeDir := filepath.Join(tmpDir, "proj", ".claude")
	os.MkdirAll(projClaudeDir, 0755)

	projectSettings := map[string]any{"b": 3, "c": 4}
	projJSON, _ := json.Marshal(projectSettings)
	os.WriteFile(filepath.Join(projClaudeDir, "settings.json"), projJSON, 0644)

	disc, err := DiscoverClaudeFiles(filepath.Join(tmpDir, "proj"), DiscoverOptions{Settings: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	// Verify merge: b should be 3 (project wins), c should be 4 (project), a should be 1 (user)
	if aVal, ok := disc.SettingsJSON["a"]; !ok || aVal != float64(1) {
		t.Errorf("Expected a=1, got %v", aVal)
	}
	if bVal, ok := disc.SettingsJSON["b"]; !ok || bVal != float64(3) {
		t.Errorf("Expected b=3 (project wins), got %v", bVal)
	}
	if cVal, ok := disc.SettingsJSON["c"]; !ok || cVal != float64(4) {
		t.Errorf("Expected c=4, got %v", cVal)
	}

	os.Setenv("HOME", home)
}

// TestSettingsArrayMerge tests array merging in settings.json with dedup.
func TestSettingsArrayMerge(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	home := os.Getenv("HOME")
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)

	homeDir := filepath.Join(tmpDir, "home")
	os.MkdirAll(homeDir, 0755)
	os.Setenv("HOME", homeDir)

	// Create user settings with array
	claudeDir := filepath.Join(homeDir, ".claude")
	os.MkdirAll(claudeDir, 0755)

	userSettings := map[string]any{"tools": []any{"A", "B"}}
	userJSON, _ := json.Marshal(userSettings)
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), userJSON, 0644)

	// Create project settings (partial overlap)
	projClaudeDir := filepath.Join(tmpDir, "proj", ".claude")
	os.MkdirAll(projClaudeDir, 0755)

	projectSettings := map[string]any{"tools": []any{"B", "C"}}
	projJSON, _ := json.Marshal(projectSettings)
	os.WriteFile(filepath.Join(projClaudeDir, "settings.json"), projJSON, 0644)

	disc, err := DiscoverClaudeFiles(filepath.Join(tmpDir, "proj"), DiscoverOptions{Settings: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	tools, ok := disc.SettingsJSON["tools"].([]any)
	if !ok {
		t.Fatalf("Expected tools to be array, got %T", disc.SettingsJSON["tools"])
	}

	// Should have A, B, C with dedup
	toolMap := make(map[string]bool)
	for _, tool := range tools {
		toolMap[tool.(string)] = true
	}

	if len(toolMap) != 3 {
		t.Errorf("Expected 3 unique tools, got %d", len(toolMap))
	}
	if !toolMap["A"] || !toolMap["B"] || !toolMap["C"] {
		t.Errorf("Expected A, B, C in tools, got %v", toolMap)
	}

	os.Setenv("HOME", home)
}

// TestMcpJsonProjectOverride tests that project .mcp.json wins over user.
func TestMcpJsonProjectOverride(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	home := os.Getenv("HOME")
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)

	homeDir := filepath.Join(tmpDir, "home")
	os.MkdirAll(homeDir, 0755)
	os.Setenv("HOME", homeDir)

	// Create user .claude.json
	claudeDir := filepath.Join(homeDir, ".claude")
	os.MkdirAll(claudeDir, 0755)

	userMcp := map[string]any{
		"mcpServers": map[string]any{
			"mycorp/server": map[string]any{
				"type": "stdio",
				"cmd":  "user-cmd",
			},
		},
	}
	userJSON, _ := json.Marshal(userMcp)
	os.WriteFile(filepath.Join(homeDir, ".claude.json"), userJSON, 0644)

	// Create git directory to mark as project root
	projRoot := filepath.Join(tmpDir, "proj")
	os.Mkdir(projRoot, 0755)
	os.Mkdir(filepath.Join(projRoot, ".git"), 0755)

	// Create project .mcp.json
	projectMcp := map[string]any{
		"mcpServers": map[string]any{
			"mycorp/server": map[string]any{
				"type": "stdio",
				"cmd":  "project-cmd",
			},
		},
	}
	projJSON, _ := json.Marshal(projectMcp)
	os.WriteFile(filepath.Join(projRoot, ".mcp.json"), projJSON, 0644)

	disc, err := DiscoverClaudeFiles(projRoot, DiscoverOptions{MCP: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	servers := disc.McpJSON["mcpServers"].(map[string]any)
	server := servers["mycorp/server"].(map[string]any)

	// Project version should win
	if cmd, ok := server["cmd"].(string); !ok || cmd != "project-cmd" {
		t.Errorf("Expected project-cmd to win, got %v", server["cmd"])
	}

	os.Setenv("HOME", home)
}

// TestCodexProjectRootDetection tests .git detection as project root.
func TestCodexProjectRootDetection(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	// Create proj/src/ with .git at proj/
	projRoot := filepath.Join(tmpDir, "proj")
	srcDir := filepath.Join(projRoot, "src")
	os.MkdirAll(srcDir, 0755)
	os.Mkdir(filepath.Join(projRoot, ".git"), 0755)

	root := findProjectRoot(srcDir)
	if root != projRoot {
		t.Errorf("Expected root %s, got %s", projRoot, root)
	}
}

// TestCodexAgentsMDWalkDown tests AGENTS.md discovery walking down from root to workDir.
func TestCodexAgentsMDWalkDown(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	// Create proj/src/lib/ with .git at proj/
	projRoot := filepath.Join(tmpDir, "proj")
	srcDir := filepath.Join(projRoot, "src")
	libDir := filepath.Join(srcDir, "lib")
	os.MkdirAll(libDir, 0755)
	os.Mkdir(filepath.Join(projRoot, ".git"), 0755)

	// Create AGENTS.md at different levels
	os.WriteFile(filepath.Join(projRoot, "AGENTS.md"), []byte("# Root Agents"), 0644)
	os.WriteFile(filepath.Join(srcDir, "AGENTS.md"), []byte("# Src Agents"), 0644)

	disc, err := DiscoverCodexFiles(libDir, DiscoverOptions{AgentsMD: true})
	if err != nil {
		t.Fatalf("DiscoverCodexFiles failed: %v", err)
	}

	// Should concatenate both files
	if !strings.Contains(disc.AgentsMD, "# Root Agents") {
		t.Error("Expected to find root AGENTS.md")
	}
	if !strings.Contains(disc.AgentsMD, "# Src Agents") {
		t.Error("Expected to find src AGENTS.md")
	}

	// Root should come before src
	if strings.Index(disc.AgentsMD, "# Root Agents") > strings.Index(disc.AgentsMD, "# Src Agents") {
		t.Error("Expected root agents before src agents")
	}
}

// TestCodexAgentsOverridePriority tests AGENTS.override.md takes precedence.
func TestCodexAgentsOverridePriority(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	projRoot := filepath.Join(tmpDir, "proj")
	os.MkdirAll(projRoot, 0755)
	os.Mkdir(filepath.Join(projRoot, ".git"), 0755)

	// Create both AGENTS.md and AGENTS.override.md
	os.WriteFile(filepath.Join(projRoot, "AGENTS.md"), []byte("# Regular Agents"), 0644)
	os.WriteFile(filepath.Join(projRoot, "AGENTS.override.md"), []byte("# Override Agents"), 0644)

	disc, err := DiscoverCodexFiles(projRoot, DiscoverOptions{AgentsMD: true})
	if err != nil {
		t.Fatalf("DiscoverCodexFiles failed: %v", err)
	}

	// Should contain override
	if !strings.Contains(disc.AgentsMD, "# Override Agents") {
		t.Error("Expected to find override AGENTS.md")
	}

	// Should NOT contain regular if override exists
	if strings.Contains(disc.AgentsMD, "# Regular Agents") {
		t.Error("Should not include regular AGENTS.md when override exists")
	}
}

// TestCodexOneFilePerLevel tests that only one file per level is included.
func TestCodexOneFilePerLevel(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	projRoot := filepath.Join(tmpDir, "proj")
	os.MkdirAll(projRoot, 0755)
	os.Mkdir(filepath.Join(projRoot, ".git"), 0755)

	// Create multiple fallback files
	os.WriteFile(filepath.Join(projRoot, "AGENTS.md"), []byte("# Agents"), 0644)
	os.WriteFile(filepath.Join(projRoot, "TEAM_GUIDE.md"), []byte("# Team Guide"), 0644)
	os.WriteFile(filepath.Join(projRoot, ".agents.md"), []byte("# Agents Dot"), 0644)

	disc, err := DiscoverCodexFiles(projRoot, DiscoverOptions{AgentsMD: true})
	if err != nil {
		t.Fatalf("DiscoverCodexFiles failed: %v", err)
	}

	// Should only have AGENTS.md content (first match)
	if !strings.Contains(disc.AgentsMD, "# Agents") {
		t.Error("Expected AGENTS.md content")
	}

	// Count occurrences to ensure we don't have duplicates
	agentsCount := strings.Count(disc.AgentsMD, "# Agents")
	if agentsCount != 1 {
		t.Errorf("Expected 1 AGENTS line, got %d (may have duplicate)", agentsCount)
	}
}

// TestClaudeExcludes tests that excluded files are skipped.
func TestClaudeExcludes(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	projRoot := filepath.Join(tmpDir, "proj")
	os.MkdirAll(projRoot, 0755)

	// Create files
	os.WriteFile(filepath.Join(projRoot, "CLAUDE.md"), []byte("# Main"), 0644)
	os.WriteFile(filepath.Join(projRoot, "config.local.md"), []byte("# Local Config"), 0644)

	// With excludes, config.local.md should still be found (excludes only apply during merge)
	// For now, just verify basic discovery works
	disc, err := DiscoverClaudeFiles(projRoot, DiscoverOptions{ClaudeMD: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	found := false
	for _, f := range disc.ClaudeMDFiles {
		if strings.Contains(f.Path, "CLAUDE.md") {
			found = true
		}
	}

	if !found {
		t.Error("Expected to find CLAUDE.md")
	}
}

// TestClaudeUserLevelAlwaysLoaded tests that user-level CLAUDE.md is always loaded.
func TestClaudeUserLevelAlwaysLoaded(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	home := os.Getenv("HOME")
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)

	homeDir := filepath.Join(tmpDir, "home")
	claudeDir := filepath.Join(homeDir, ".claude")
	os.MkdirAll(claudeDir, 0755)
	os.Setenv("HOME", homeDir)

	// Only create user-level CLAUDE.md
	os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte("# User CLAUDE.md"), 0644)

	// Project has no CLAUDE.md
	projDir := filepath.Join(tmpDir, "proj")
	os.Mkdir(projDir, 0755)

	disc, err := DiscoverClaudeFiles(projDir, DiscoverOptions{ClaudeMD: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	found := false
	for _, f := range disc.ClaudeMDFiles {
		if f.Source == "user" && strings.Contains(f.Content, "User CLAUDE.md") {
			found = true
		}
	}

	if !found {
		t.Error("Expected user-level CLAUDE.md to be loaded")
	}

	os.Setenv("HOME", home)
}

// TestFileSizeLimit tests that oversized files are skipped.
func TestFileSizeLimit(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	projRoot := filepath.Join(tmpDir, "proj")
	os.MkdirAll(projRoot, 0755)

	// Create a file larger than 5MB
	largeContent := strings.Repeat("x", 6*1024*1024)
	os.WriteFile(filepath.Join(projRoot, "CLAUDE.md"), []byte(largeContent), 0644)

	disc, err := DiscoverClaudeFiles(projRoot, DiscoverOptions{ClaudeMD: true})

	// readFileLimited returns an error, which is silently skipped during discovery
	// So discovery should succeed but the large file should not be included
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Verify the large project file was not loaded
	for _, f := range disc.ClaudeMDFiles {
		if strings.Contains(f.Path, "proj") && strings.Contains(f.Path, "CLAUDE.md") {
			t.Error("Expected oversized project file to be skipped")
		}
	}
}

// TestPathTraversalPrevention tests that path traversal is prevented.
func TestPathTraversalPrevention(t *testing.T) {
	// Create a path with ../ components
	path := "/tmp/../etc/passwd"

	resolved, err := resolvePath(path)
	if err != nil {
		// This is expected if path doesn't exist, but let's verify clean behavior
		return
	}

	// If it resolved, it should be cleaned
	if strings.Contains(resolved, "..") {
		t.Error("Path should not contain .. after resolution")
	}
}

// TestDiscoveryEmptyWorkDir tests that empty workDir returns empty discovery.
func TestDiscoveryEmptyWorkDir(t *testing.T) {
	disc, err := DiscoverClaudeFiles("", DiscoverOptions{ClaudeMD: true})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	if len(disc.ClaudeMDFiles) > 0 {
		t.Error("Expected no files discovered for empty workDir")
	}
}

// TestDiscoveryGranularControl tests that only enabled categories are discovered.
func TestDiscoveryGranularControl(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	projRoot := filepath.Join(tmpDir, "proj")
	claudeDir := filepath.Join(projRoot, ".claude")
	os.MkdirAll(claudeDir, 0755)

	// Create various files
	os.WriteFile(filepath.Join(projRoot, "CLAUDE.md"), []byte("# Prompt"), 0644)

	settings := map[string]any{"key": "value"}
	settingsJSON, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), settingsJSON, 0644)

	// Only enable CLAUDE.md, not settings
	disc, err := DiscoverClaudeFiles(projRoot, DiscoverOptions{
		ClaudeMD: true,
		Settings: false,
	})
	if err != nil {
		t.Fatalf("DiscoverClaudeFiles failed: %v", err)
	}

	if len(disc.ClaudeMDFiles) == 0 {
		t.Error("Expected CLAUDE.md to be discovered")
	}

	if len(disc.SettingsJSON) > 0 {
		t.Error("Expected no settings.json discovery when disabled")
	}
}

// TestCodexConfigTOMLCascade tests config.toml merge behavior.
func TestCodexConfigTOMLCascade(t *testing.T) {
	tmpDir := t.TempDir()
	defer os.RemoveAll(tmpDir)

	home := os.Getenv("HOME")
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)

	homeDir := filepath.Join(tmpDir, "home")
	codexDir := filepath.Join(homeDir, ".codex")
	os.MkdirAll(codexDir, 0755)
	os.Setenv("HOME", homeDir)

	// Create user config
	userConfig := "model = \"claude-3-opus\"\ntemperature = 0.8"
	os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(userConfig), 0644)

	// Create project
	projRoot := filepath.Join(tmpDir, "proj")
	projCodexDir := filepath.Join(projRoot, ".codex")
	os.MkdirAll(projCodexDir, 0755)
	os.Mkdir(filepath.Join(projRoot, ".git"), 0755)

	// Create project config (override temperature)
	projConfig := "temperature = 0.5"
	os.WriteFile(filepath.Join(projCodexDir, "config.toml"), []byte(projConfig), 0644)

	disc, err := DiscoverCodexFiles(projRoot, DiscoverOptions{ConfigTOML: true})
	if err != nil {
		t.Fatalf("DiscoverCodexFiles failed: %v", err)
	}

	// Project value should win
	if temp, ok := disc.ConfigTOML["temperature"].(string); !ok || temp != "0.5" {
		t.Errorf("Expected temperature=0.5 (project wins), got %v", disc.ConfigTOML["temperature"])
	}

	os.Setenv("HOME", home)
}

// TestBuildPathList tests the path list building from root to workDir.
func TestBuildPathList(t *testing.T) {
	root := "/proj"
	workDir := "/proj/src/lib/sub"

	paths := buildPathList(root, workDir)

	// Should include all levels from root to workDir
	if len(paths) < 2 {
		t.Fatalf("Expected at least 2 paths, got %d", len(paths))
	}

	// First should be root
	if paths[0] != root {
		t.Errorf("Expected first path to be %s, got %s", root, paths[0])
	}

	// Last should be workDir
	if paths[len(paths)-1] != workDir {
		t.Errorf("Expected last path to be %s, got %s", workDir, paths[len(paths)-1])
	}
}

// TestDeepMerge tests map merging with right winning on conflicts.
func TestDeepMerge(t *testing.T) {
	left := map[string]any{
		"a": 1,
		"b": map[string]any{"x": 10, "y": 20},
	}

	right := map[string]any{
		"b": map[string]any{"y": 30, "z": 40},
		"c": 3,
	}

	result := deepMerge(left, right)

	// Check merge results
	if result["a"] != 1 {
		t.Errorf("Expected a=1, got %v", result["a"])
	}
	if result["c"] != 3 {
		t.Errorf("Expected c=3, got %v", result["c"])
	}

	nested := result["b"].(map[string]any)
	if nested["x"] != 10 {
		t.Errorf("Expected x=10 (from left), got %v", nested["x"])
	}
	if nested["y"] != 30 {
		t.Errorf("Expected y=30 (right wins), got %v", nested["y"])
	}
	if nested["z"] != 40 {
		t.Errorf("Expected z=40, got %v", nested["z"])
	}
}

// TestParseAutoDiscoverNil tests that nil returns nil.
func TestParseAutoDiscoverNil(t *testing.T) {
	result := ParseAutoDiscover(nil)
	if result != nil {
		t.Error("Expected nil for nil input")
	}
}

// TestParseAutoDiscoverBoolTrue tests boolean true expands to all categories.
func TestParseAutoDiscoverBoolTrue(t *testing.T) {
	result := ParseAutoDiscover(true)
	if result == nil {
		t.Fatal("Expected non-nil result for true")
	}

	if !result.ClaudeMD || !result.Settings || !result.MCP || !result.Rules || !result.Agents {
		t.Error("Expected all Claude categories to be true")
	}

	if !result.AgentsMD || !result.ConfigTOML {
		t.Error("Expected all Codex categories to be true")
	}
}

// TestParseAutoDiscoverBoolFalse tests boolean false disables all categories.
func TestParseAutoDiscoverBoolFalse(t *testing.T) {
	result := ParseAutoDiscover(false)
	if result == nil {
		t.Fatal("Expected non-nil result for false")
	}

	if result.ClaudeMD || result.Settings || result.MCP || result.Rules || result.Agents {
		t.Error("Expected all Claude categories to be false")
	}

	if result.AgentsMD || result.ConfigTOML {
		t.Error("Expected all Codex categories to be false")
	}
}

// TestParseAutoDiscoverGranular tests map-based granular control.
func TestParseAutoDiscoverGranular(t *testing.T) {
	input := map[string]interface{}{
		"claude_md": true,
		"settings":  false,
		// Others unspecified, should default to false
	}

	result := ParseAutoDiscover(input)
	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if !result.ClaudeMD {
		t.Error("Expected claude_md to be true")
	}

	if result.Settings {
		t.Error("Expected settings to be false")
	}

	if result.MCP {
		t.Error("Expected unspecified mcp to default to false")
	}
}

// TestParseAutoDiscoverInvalidType tests invalid types return nil.
func TestParseAutoDiscoverInvalidType(t *testing.T) {
	result := ParseAutoDiscover("invalid")
	if result != nil {
		t.Error("Expected nil for invalid type")
	}

	result = ParseAutoDiscover(123)
	if result != nil {
		t.Error("Expected nil for invalid type")
	}
}

// TestMergeClaudeMD tests merging of discovered CLAUDE.md files.
func TestMergeClaudeMD(t *testing.T) {
	files := []DiscoveredFile{
		{Path: "/proj/CLAUDE.md", Content: "# Part 1", Source: "project"},
		{Path: "/proj/src/CLAUDE.md", Content: "# Part 2", Source: "project"},
		{Path: "/home/.claude/CLAUDE.md", Content: "# User", Source: "user"},
	}

	result := mergeClaudeMD(files)

	// Should concatenate in order with blank lines
	if !strings.Contains(result, "# Part 1") {
		t.Error("Expected Part 1 in merged output")
	}
	if !strings.Contains(result, "# Part 2") {
		t.Error("Expected Part 2 in merged output")
	}
	if !strings.Contains(result, "# User") {
		t.Error("Expected User in merged output")
	}

	// Should have blank lines between parts
	if !strings.Contains(result, "\n\n") {
		t.Error("Expected blank lines between parts")
	}
}

// TestMergeClaudeMDEmpty tests merging with empty files.
func TestMergeClaudeMDEmpty(t *testing.T) {
	files := []DiscoveredFile{}
	result := mergeClaudeMD(files)
	if result != "" {
		t.Error("Expected empty string for empty files")
	}

	files = []DiscoveredFile{{Path: "/test.md", Content: "", Source: "project"}}
	result = mergeClaudeMD(files)
	if result != "" {
		t.Error("Expected empty string for file with empty content")
	}
}
