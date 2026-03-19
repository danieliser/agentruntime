# Configuration Auto-Discovery Specification

## Problem

Docker-based agent sessions run in isolation without access to the host filesystem. When agents spawn in containers, they lose critical project context:

- No project-level `.claude/CLAUDE.md` or `CLAUDE.md` files
- No project-scoped `settings.json` cascade (only user/system defaults apply)
- No project-level `.mcp.json` MCP server registrations
- No `.claude/rules/` or `.claude/agents/` project customizations
- No Codex project-level config.toml or instruction files

Result: Agents run blind to project conventions, authentication, and tool integrations.

## Solution

Auto-discovery replicates the EXACT file resolution behavior of native Claude Code and Codex CLIs, ensuring agents in Docker containers have identical configuration as they would on the host.

## Discovery Engine

Three phases:

1. **Discovery** — Scan filesystem from work_dir upward/downward, collecting candidate files per agent
2. **Resolution** — Read and parse files, follow imports, apply merging rules
3. **Materialization** — Write discovered files into the session home directory (pkg/materialize)

Discovery is agent-specific and only triggered when `auto_discover` is true (default for Docker, always true for local).

---

## Claude Code Discovery Rules

### CLAUDE.md Files

**Search strategy:** Walk UP from work_dir to filesystem root, checking at each level.

At each directory level D from work_dir upward:
1. Check for `D/CLAUDE.md`
2. Check for `D/.claude/CLAUDE.md`
3. Continue upward if neither found

**Collection:** Collect ALL found files (no override — concat later).

**User-level files (always loaded):**
- `~/.claude/CLAUDE.md` (last priority; loaded after project files)

**Managed policy (platform-specific, lowest priority):**
- macOS: `/Library/Application Support/ClaudeCode/CLAUDE.md`
- Linux: `/etc/claude-code/CLAUDE.md`

**Import syntax:** Files can contain `@path/to/import` directives (max 5 hops, circular reference detection required).

**Exclusions:** Respect `claudeMdExcludes` setting (glob patterns, checked against relative path from project root).

**Final order (merge preference, right wins):**
```
[system-policy] → [user-level] → [project-root-down] → [work-dir-immediate]
```

### settings.json Cascade

**Locations (4 scopes, local always wins):**

1. **Project-local** (gitignored): `.claude/settings.local.json`
2. **Project-committed**: `.claude/settings.json`
3. **User**: `~/.claude/settings.json`
4. **System managed policy**:
   - macOS: `/Library/Application Support/ClaudeCode/settings.json`
   - Linux: `/etc/claude-code/settings.json`

**Merge strategy:** Deep merge at each level. Arrays merge (right appends to left, dedup by value).

**Resolution order:**
- Load all 4 scopes
- Start with system policy as base
- Deep-merge user level
- Deep-merge project-committed
- Deep-merge project-local (highest priority)

### .mcp.json

**Location:** `.mcp.json` at project root (work_dir or closest ancestor containing .git/.hg/.sl).

**User-level:** `~/.claude.json` (contains `mcpServers` key).

**Merge:** Project .mcp.json wins over user ~/.claude.json for same server name.

**Scope approval:** Project-scoped MCP servers require explicit approval flag (recorded in Claude state, not auto-approved).

### .claude/rules/ Directory

**Discovery:** Recursive walk of `.claude/rules/` directory tree.

**Two modes:**
- **Unconditional files:** Files without YAML frontmatter `paths:` — loaded always
- **Conditional files:** Files with `paths:` frontmatter — loaded only if current file path matches glob

**Load order:**
1. User rules (`~/.claude/rules/**/*.md`)
2. Project rules (`.claude/rules/**/*.md`)

**Path matching:** Use glob syntax; relative to work_dir.

### .claude/agents/ Directory

**Discovery:** Scan for agent customizations.

**Locations:**
- Project: `.claude/agents/`
- User: `~/.claude/agents/`

**Precedence:** Project wins on name collision.

---

## Codex CLI Discovery Rules

### AGENTS.md Files

**Project root detection:** Walk UP from work_dir looking for:
- `.git/`
- `.hg/`
- `.sl/` (Sapling)
- Custom markers (configurable via `project_root_markers` in config.toml)

Stop at first match; treat as project root.

**Search strategy:** Walk DOWN from project root to work_dir, checking at each level.

At each directory D from project-root downward to work_dir:
1. Check for `D/AGENTS.override.md` (exact name, override wins)
2. If not found, check `D/AGENTS.md` (fallback)
3. If neither found, check fallbacks in order:
   - `D/TEAM_GUIDE.md`
   - `D/.agents.md`
4. **Include at most ONE file per directory level** (first match wins)

**Collection:** Concatenate root-down with blank lines between levels.

**User-level:** `~/.codex/AGENTS.override.md` or `~/.codex/AGENTS.md` (appended last).

**Result:** Single concatenated markdown string, root-to-workdir, preserving directory hierarchy.

### config.toml Cascade

**Locations (5 levels, CLI flags win):**

1. **CLI flags** (highest priority)
2. **Profile values** (from `~/.codex/profiles.toml`)
3. **Project** (`.codex/config.toml`) — closest to work_dir wins; must be explicitly marked "trusted" in config
4. **User** (`~/.codex/config.toml`)
5. **System** (`/etc/codex/config.toml`) (lowest priority)

**Trust model:** Project config.toml only loaded if:
- Explicitly allowed via `trusted_projects` list in user config, OR
- User accepts inline confirmation prompt (not used in Docker mode)

**Merge strategy:** Same as Claude — deep merge, arrays append with dedup.

---

## Materializer Integration

File discovery happens BEFORE calling `Materialize()` in `pkg/materialize/materializer.go`.

### New Discovery Functions

**Location:** `pkg/materialize/discover.go` (new file)

```go
// DiscoverClaudeFiles returns discovered Claude configuration files.
// Discovers CLAUDE.md, settings.json cascade, .mcp.json, rules, and agents.
func DiscoverClaudeFiles(workDir string) (*ClaudeDiscovery, error)

// DiscoverCodexFiles returns discovered Codex configuration files.
// Discovers AGENTS.md, config.toml cascade, project root, and instructions.
func DiscoverCodexFiles(workDir string) (*CodexDiscovery, error)
```

### Discovery Structs

```go
type ClaudeDiscovery struct {
	ClaudeMDFiles   []DiscoveredFile   // All found CLAUDE.md files (path, content, source)
	SettingsJSON    map[string]any     // Merged settings cascade
	McpJSON         map[string]any     // Merged .mcp.json
	RulesFiles      []DiscoveredFile   // All .claude/rules/**/*.md files
	AgentsDir       string             // Path to .claude/agents/ if exists
	ProjectRoot     string             // Detected project root (.git/.hg/.sl)
}

type CodexDiscovery struct {
	AgentsMD        string             // Concatenated AGENTS.md root-to-work_dir
	ConfigTOML      map[string]any     // Merged config.toml cascade
	InstructionsMD  string             // instructions.md if exists
	ProjectRoot     string             // Detected project root
	TrustLevel      string             // "trusted" or "untrusted"
}

type DiscoveredFile struct {
	Path    string // Absolute path on host
	Content string // Full file contents
	Source  string // "project" | "user" | "system" | "profile"
	Level   int    // Directory level (0 = work_dir, up for Claude; root for Codex)
}
```

### Materialization Precedence

**Explicit SessionRequest fields always win over discovered files:**

```go
// In Materialize():
if req.Claude != nil {
	discovered, _ := DiscoverClaudeFiles(workDir)

	// Explicit content overrides discovery
	if req.Claude.ClaudeMD == "" && discovered.ClaudeMDFiles != nil {
		req.Claude.ClaudeMD = mergeClaudeMD(discovered.ClaudeMDFiles)
	}

	if req.Claude.SettingsJSON == nil {
		req.Claude.SettingsJSON = discovered.SettingsJSON
	} else {
		// Deep merge: explicit wins, but discovered fills gaps
		req.Claude.SettingsJSON = deepMerge(discovered.SettingsJSON, req.Claude.SettingsJSON)
	}
}
```

---

## SessionRequest API

New field in `pkg/api/schema/types.go`:

```go
type SessionRequest struct {
	// ... existing fields ...

	// AutoDiscover controls configuration auto-discovery from the filesystem.
	// Supports three forms:
	//
	// 1. bool (shorthand):
	//    true  — enable ALL discovery categories
	//    false — disable ALL discovery (explicit config only)
	//    nil/unset — platform default (true for Docker, true for local)
	//
	// 2. map[string]bool (granular):
	//    {"claude_md": true, "settings": false, ...}
	//    All unspecified keys default to false (opt-in per category)
	//
	// Valid category keys:
	//   Claude:  "claude_md", "settings", "mcp", "rules", "agents"
	//   Codex:   "agents_md", "config_toml"
	//
	// Example: {"claude_md": true} enables ONLY CLAUDE.md discovery
	AutoDiscover interface{} `json:"auto_discover,omitempty" yaml:"auto_discover,omitempty"`
}
```

### AutoDiscover Interpretation

**Form 1: Boolean (Shorthand)**
```json
{
  "auto_discover": true    // All categories enabled (full discovery)
}
```
```json
{
  "auto_discover": false   // All categories disabled (explicit config only)
}
```
```json
{
  // unset/null           // Platform default: true for Docker, true for local
}
```

**Form 2: Object (Granular)**
```json
{
  "auto_discover": {
    "claude_md": true,
    "settings": false,
    "mcp": false,
    "rules": false,
    "agents": false
  }
}
```

When object form is provided:
- All unspecified keys default to **false** (whitelist/opt-in model)
- Each category controls discovery of a specific file type
- Explicit SessionRequest fields always take precedence over discovered values

### Discovery Categories

**Claude:**
- `claude_md` — CLAUDE.md walk-up (all found files, concatenated)
- `settings` — settings.json cascade (system → user → project-committed → project-local)
- `mcp` — .mcp.json merge and user ~/.claude.json servers
- `rules` — .claude/rules/ recursive discovery
- `agents` — .claude/agents/ directory discovery

**Codex:**
- `agents_md` — AGENTS.md walk-down (root-to-work_dir concatenation)
- `config_toml` — config.toml cascade (system → user → project, with trust check)

### Common Use Cases

**1. Full Discovery (Deploy Agent with Project Context)**
```json
{
  "agent": "claude",
  "auto_discover": true,
  "work_dir": "/path/to/project"
}
```
Result: All Claude categories enabled; settings, MCP servers, rules, agents merged.

**2. Instructions Only (No Settings/Tools)**
```json
{
  "agent": "claude",
  "auto_discover": {"claude_md": true},
  "work_dir": "/path/to/project"
}
```
Result: Only CLAUDE.md discovered. No settings.json, MCP servers, or agent definitions from filesystem.

**3. Everything Except MCP (Avoid Host Tools)**
```json
{
  "agent": "claude",
  "auto_discover": {
    "claude_md": true,
    "settings": true,
    "rules": true,
    "agents": true,
    "mcp": false
  },
  "work_dir": "/path/to/project"
}
```
Result: Full context except MCP server registrations.

**4. No Discovery (Explicit Config Only)**
```json
{
  "agent": "claude",
  "auto_discover": false,
  "claude": {
    "claude_md": "# Custom prompt only",
    "settings_json": {"...": "..."}
  },
  "work_dir": "/path/to/project"
}
```
Result: Only SessionRequest.Claude fields used; filesystem discovery skipped.

**5. Codex: Context + Config Only**
```json
{
  "agent": "codex",
  "auto_discover": {
    "agents_md": true,
    "config_toml": false
  },
  "work_dir": "/path/to/project"
}
```
Result: AGENTS.md instructions discovered; config.toml (with trust implications) skipped.

### Parsing & Validation

**In Materialize():**

```go
// Helper to parse auto_discover field
func parseAutoDiscover(raw interface{}) map[string]bool {
	categories := make(map[string]bool)

	switch v := raw.(type) {
	case nil:
		// Platform default (handled in caller)
		return nil
	case bool:
		// Shorthand: apply to all categories
		return map[string]bool{
			"claude_md": v, "settings": v, "mcp": v, "rules": v, "agents": v,
			"agents_md": v, "config_toml": v,
		}
	case map[string]interface{}:
		// Granular: all unspecified default to false
		for key := range v {
			if bval, ok := v[key].(bool); ok {
				categories[key] = bval
			}
		}
		return categories
	default:
		// Invalid type: treat as nil (platform default)
		return nil
	}
}
```

### Precedence (Explicit Always Wins)

Even when discovery is enabled, explicit SessionRequest fields take priority:

```go
// Pseudocode
if categories["claude_md"] && req.Claude.ClaudeMD == "" {
	req.Claude.ClaudeMD = discovered.ClaudeMD
}

if categories["settings"] && req.Claude.SettingsJSON == nil {
	req.Claude.SettingsJSON = discovered.SettingsJSON
} else if categories["settings"] && req.Claude.SettingsJSON != nil {
	// Deep merge: discovered fills gaps, explicit wins on conflicts
	req.Claude.SettingsJSON = deepMerge(discovered, explicit)
}
```

---

## Security & Constraints

### Path Traversal Prevention

1. **Symlink resolution:** Resolve all symlinks to canonical paths before walking
2. **Boundary check:** Ensure resolved path stays within project root or user home
3. **Parent traversal:** Strip `../` components at each level during walk

```go
func resolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)  // Resolve symlinks
	if err != nil {
		return "", err
	}

	cleaned := filepath.Clean(resolved)

	// Ensure path is absolute
	if !filepath.IsAbs(cleaned) {
		return "", errors.New("path must be absolute")
	}

	return cleaned, nil
}
```

### File Size Limits

- **Per-file:** 5 MB max (prevent memory exhaustion from large discovered files)
- **Aggregate:** 20 MB max across all discovered files for a single session

Oversized files are logged and skipped with a warning.

### Safe Import Resolution

CLAUDE.md `@import` directives:
- Max 5 hops (prevent circular/deep chains)
- Must resolve to absolute paths
- Size limit applies per imported file
- Circular reference detection (track visited paths)

### Restricted Locations

Never auto-discover from:
- `/tmp/`, `/var/tmp/` (ephemeral)
- `/sys/`, `/proc/` (system files)
- Paths containing `.git/` inside discovered content (prevents leaking private keys)

### Exclusion Patterns

Claude `claudeMdExcludes` setting supports glob patterns:

```json
{
  "claudeMdExcludes": [
    "**/node_modules/**",
    "**/.venv/**",
    "**/*.local.md"
  ]
}
```

Applied against relative path from project root.

---

## Testing Plan

### Test Cases: Claude Discovery

**1. CLAUDE.md Walk-Up**
- Given: work_dir = `/proj/src/lib/`, files at `/proj/CLAUDE.md`, `/proj/src/CLAUDE.md`
- Expected: Both found, root-first in merge order
- Test: `TestClaudeDiscoveryWalkUp`

**2. .claude/CLAUDE.md Alternate Path**
- Given: `.claude/CLAUDE.md` at project root instead of root CLAUDE.md
- Expected: Found and included in merge
- Test: `TestClaudeDiscoveryCladueSubdir`

**3. settings.json Deep Merge**
- Given: System has `{"a": 1}`, user has `{"a": 2, "b": 3}`, project has `{"b": 4, "c": 5}`
- Expected: Merged result `{"a": 2, "b": 4, "c": 5}`
- Test: `TestSettingsDeepMerge`

**4. Array Merge in settings.json**
- Given: System `{"tools": ["A", "B"]}`, user `{"tools": ["B", "C"]}`
- Expected: `{"tools": ["A", "B", "C"]}` (with dedup)
- Test: `TestSettingsArrayMerge`

**5. .mcp.json Override**
- Given: User has `mycorp/server`, project has `mycorp/server` with different config
- Expected: Project version wins
- Test: `TestMcpJsonProjectOverride`

**6. @import Resolution**
- Given: CLAUDE.md contains `@../.claude/common.md`
- Expected: common.md loaded and inserted, max 5 hops
- Test: `TestClaudeImportResolution`

**7. @import Circular Reference**
- Given: a.md imports b.md, b.md imports a.md
- Expected: Error or max-depth cutoff, no infinite loop
- Test: `TestClaudeImportCircularRef`

**8. claudeMdExcludes Glob**
- Given: `claudeMdExcludes: ["**/*.local.md"]`, work_dir has `config.local.md`
- Expected: File skipped
- Test: `TestClaudeExcludes`

**9. User-Level Always Loaded**
- Given: No project CLAUDE.md, user has `~/.claude/CLAUDE.md`
- Expected: User file included
- Test: `TestClaudeUserLevelAlwaysLoaded`

**10. Symlink Following**
- Given: CLAUDE.md is symlink to /other/path/file.md
- Expected: Followed and resolved
- Test: `TestClaudeSymlinkResolution`

### Test Cases: Codex Discovery

**1. Project Root Detection**
- Given: work_dir = `/proj/src/`, .git at `/proj/.git/`
- Expected: Project root detected as `/proj/`
- Test: `TestCodexProjectRootDetection`

**2. AGENTS.md Walk-Down**
- Given: Project root `/proj/`, work_dir `/proj/src/lib/`, files at `/proj/AGENTS.md`, `/proj/src/AGENTS.md`
- Expected: Both found, concatenated root-first with blank line
- Test: `TestCodexAgentsMDWalkDown`

**3. AGENTS.override.md Priority**
- Given: Both `AGENTS.md` and `AGENTS.override.md` in same dir
- Expected: override.md wins, AGENTS.md skipped
- Test: `TestCodexAgentsOverridePriority`

**4. One File Per Level**
- Given: Same dir has AGENTS.md, TEAM_GUIDE.md, .agents.md
- Expected: Only AGENTS.md included (first match)
- Test: `TestCodexOneFilePerLevel`

**5. Fallback Chain**
- Given: AGENTS.md missing, TEAM_GUIDE.md present
- Expected: TEAM_GUIDE.md included
- Test: `TestCodexFallbackChain`

**6. config.toml Cascade**
- Given: System, user, project all have config.toml with different values
- Expected: Merged with project > user > system
- Test: `TestCodexConfigTomlCascade`

**7. config.toml Trust Check**
- Given: Project config.toml but project NOT in trusted_projects list
- Expected: Skipped (untrusted), only user/system used
- Test: `TestCodexConfigTomlTrustCheck`

**8. User AGENTS.md Override**
- Given: Project AGENTS.md + user `~/.codex/AGENTS.override.md`
- Expected: Both found and concatenated, user last
- Test: `TestCodexUserAgentsOverride`

### Test Cases: Integration

**1. Materialization with Discovery**
- Given: SessionRequest with auto_discover=true, no explicit Claude.ClaudeMD
- Expected: Discovered CLAUDE.md written to ~/.claude/CLAUDE.md in container
- Test: `TestMaterializeWithDiscovery`

**2. Explicit Wins Discovery**
- Given: auto_discover=true, but SessionRequest.Claude.ClaudeMD is set
- Expected: Explicit value used, discovery skipped for that field
- Test: `TestMaterializeExplicitWinsDiscovery`

**3. Auto-Discover Disabled**
- Given: auto_discover=false
- Expected: No discovery, only explicit fields used
- Test: `TestMaterializeAutoDiscoverDisabled`

**4. Discovery with Docker Runtime**
- Given: Docker runtime, auto_discover unset
- Expected: Defaults to true, discovery happens
- Test: `TestDockerAutoDiscoverDefault`

**5. Discovery with Local Runtime**
- Given: Local runtime, auto_discover unset
- Expected: Defaults to true, discovery happens
- Test: `TestLocalAutoDiscoverDefault`

**6. Granular: Claude MD Only**
- Given: auto_discover={"claude_md": true}, no explicit Claude.ClaudeMD
- Expected: CLAUDE.md discovered, settings/mcp/rules/agents skipped
- Test: `TestMaterializeGranularClaudeMDOnly`

**7. Granular: Everything Except MCP**
- Given: auto_discover={"claude_md": true, "settings": true, "mcp": false, "rules": true, "agents": true}
- Expected: CLAUDE.md, settings, rules, agents discovered; .mcp.json skipped
- Test: `TestMaterializeGranularExceptMCP`

**8. Granular: Codex Agents Only**
- Given: auto_discover={"agents_md": true, "config_toml": false}
- Expected: AGENTS.md discovered, config.toml skipped
- Test: `TestMaterializeCodexGranularAgentsOnly`

**9. Boolean Shorthand: True**
- Given: auto_discover=true (not an object)
- Expected: Parsed as all categories enabled
- Test: `TestAutoDiscoverBooleanTrue`

**10. Boolean Shorthand: False**
- Given: auto_discover=false (not an object)
- Expected: Parsed as all categories disabled
- Test: `TestAutoDiscoverBooleanFalse`

### Test Cases: Security

**1. Symlink Boundary Check**
- Given: CLAUDE.md symlinks to /etc/passwd
- Expected: Symlink followed, but /etc not allowed (outside home/project)
- Test: `TestSymlinkBoundaryCheck`

**2. Path Traversal Prevention**
- Given: Attempted walk with embedded `../`
- Expected: Traversal components stripped
- Test: `TestPathTraversalPrevention`

**3. File Size Limit**
- Given: 10 MB CLAUDE.md file
- Expected: Skipped with warning, session continues
- Test: `TestFileSizeLimit`

**4. Aggregate Size Limit**
- Given: 15 MB CLAUDE.md + 10 MB config.json (25 MB total, limit 20 MB)
- Expected: config.json skipped after CLAUDE.md reaches limit
- Test: `TestAggregateSizeLimit`

**5. Import Depth Limit**
- Given: 6-hop import chain (limit 5)
- Expected: 6th import fails, logged, earlier imports applied
- Test: `TestImportDepthLimit`

---

## File Locations & Precedence Reference

### Claude Code Complete Discovery Map

```
Priority (right wins):
  system-policy
  └─ /Library/Application Support/ClaudeCode/CLAUDE.md (macOS)
     /etc/claude-code/CLAUDE.md (Linux)

  user-level
  └─ ~/.claude/CLAUDE.md

  project (walk-up from work_dir)
  ├─ work_dir/CLAUDE.md
  ├─ work_dir/.claude/CLAUDE.md
  ├─ parent/CLAUDE.md
  ├─ parent/.claude/CLAUDE.md
  ├─ ... (upward to root)

  project-root-specific
  └─ {detected-project-root}/settings.json → ~/.claude/settings.json → system
```

### Codex Complete Discovery Map

```
Project Root: First of .git, .hg, .sl (walk-up)

Priority (walk-down from root to work_dir):
  project-root/AGENTS.override.md
  └─ (if not found) project-root/AGENTS.md
     └─ (if not found) project-root/TEAM_GUIDE.md or .agents.md

  ... (one per directory level, walk down to work_dir)

  work_dir/AGENTS.override.md
  └─ (if not found) work_dir/AGENTS.md
     └─ (if not found) work_dir/TEAM_GUIDE.md or .agents.md

  user-level (appended last)
  └─ ~/.codex/AGENTS.override.md or ~/.codex/AGENTS.md
```

---

## Implementation Checklist

### Phase 1: Core Discovery Engine

- [ ] Implement `pkg/materialize/discover.go` with `DiscoverClaudeFiles()` and `DiscoverCodexFiles()`
- [ ] Implement path resolution (symlinks, traversal prevention)
- [ ] Implement file reading with size limits
- [ ] Implement CLAUDE.md merge logic
- [ ] Implement settings.json deep merge
- [ ] Implement .mcp.json merge
- [ ] Add test coverage for all test cases above

### Phase 2: Codex Integration

- [ ] Implement AGENTS.md walk-down logic
- [ ] Implement project root detection (.git/.hg/.sl)
- [ ] Implement config.toml cascade and merge
- [ ] Implement trust checking
- [ ] Add test coverage

### Phase 3: Claude Advanced Features

- [ ] Implement @import resolution (max 5 hops, circular detection)
- [ ] Implement claudeMdExcludes glob matching
- [ ] Implement .claude/rules/ conditional loading
- [ ] Implement .claude/agents/ discovery
- [ ] Add test coverage

### Phase 4: Materialization Integration

- [ ] Add SessionRequest.AutoDiscover field (interface{} for bool/map flexibility)
- [ ] Implement `parseAutoDiscover()` helper (bool → map expansion, nil → default)
- [ ] Modify `Materialize()` to check category flags before calling each discovery function
- [ ] Implement precedence logic (explicit wins, discovered fills gaps for null/empty fields)
- [ ] Deep merge logic for settings.json, config.toml, .mcp.json
- [ ] Set defaults per runtime (Docker=true, Local=true) when AutoDiscover unset
- [ ] Add integration tests for all use cases (full, instructions-only, exempt-mcp, etc.)

### Phase 5: Documentation & Release

- [ ] Update README with auto-discovery behavior
- [ ] Add examples to docs/guides/
- [ ] Update sidecar docs with discovery behavior
- [ ] Release notes

---

## References

- **Existing Materializer:** `/pkg/materialize/materializer.go` (lines 80-214)
- **SessionRequest Schema:** `/pkg/api/schema/types.go` (lines 11-56)
- **Claude Code CLI Documentation:** Claude Code v2.1+ (internal docs, CLAUDE.md walk-up behavior)
- **Codex CLI Documentation:** Codex (internal docs, AGENTS.md and config.toml cascade)
- **PAOP Persistence Knowledge:** `/docs/PERSISTENCE-KNOWLEDGE-EXTRACTION.md` (section 8, agent-home pattern)
