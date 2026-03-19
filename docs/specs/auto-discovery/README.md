# Configuration Auto-Discovery Spec

This directory contains the specification for implementing configuration auto-discovery in agentruntime, replicating the EXACT file resolution behavior of Claude Code and Codex CLIs.

## Files

- **SPEC.md** — Complete specification document
  - Problem statement (agents run blind in Docker)
  - Solution overview (auto-discovery replicates native CLI behavior)
  - Exact discovery rules for Claude Code (CLAUDE.md, settings.json, .mcp.json, rules, agents)
  - Exact discovery rules for Codex (AGENTS.md, config.toml, project root detection)
  - Materializer integration points
  - SessionRequest API with granular control (boolean shorthand + map-based per-category flags)
  - Security constraints (path traversal, symlink boundaries, file size limits)
  - 30+ test cases covering all discovery paths
  - 5-phase implementation checklist

## Key Concepts

### Discovery Strategy

**Claude:** Walk UP from work_dir to filesystem root, collecting ALL found files (project override user > system).

**Codex:** Walk DOWN from project root to work_dir, including one file per level, concatenated root-to-work_dir.

### SessionRequest Control

New `AutoDiscover` field supports three forms:

1. **Boolean shorthand:**
   - `true` → All categories enabled (full discovery)
   - `false` → All categories disabled (explicit config only)
   - `null/unset` → Platform default (Docker: true, Local: true)

2. **Map (granular control):**
   ```json
   {
     "auto_discover": {
       "claude_md": true,
       "settings": false,
       "mcp": false,
       "rules": true,
       "agents": false
     }
   }
   ```
   All unspecified keys default to `false` (whitelist/opt-in).

3. **Use cases:**
   - Full context: `{"auto_discover": true}`
   - Instructions only: `{"auto_discover": {"claude_md": true}}`
   - Everything except MCP: `{"auto_discover": {"claude_md": true, "settings": true, "rules": true, "agents": true, "mcp": false}}`
   - No discovery: `{"auto_discover": false}`

### Precedence

Explicit SessionRequest fields ALWAYS win over discovered files:

```go
if req.Claude.ClaudeMD == "" && categories["claude_md"] {
    req.Claude.ClaudeMD = discovered.ClaudeMD
}

if req.Claude.SettingsJSON == nil && categories["settings"] {
    req.Claude.SettingsJSON = discovered.SettingsJSON
} else if req.Claude.SettingsJSON != nil {
    // Deep merge: discovered fills gaps, explicit wins on conflicts
    req.Claude.SettingsJSON = deepMerge(discovered, explicit)
}
```

## Security Model

- **Path traversal prevention:** Symlink resolution, boundary checks, `../` stripping
- **File size limits:** 5 MB per file, 20 MB aggregate per session
- **Import depth limit:** Max 5 hops for CLAUDE.md `@import` directives
- **Restricted locations:** Never discover from `/tmp/`, `/sys/`, `/proc/`, or paths containing private keys
- **Exclusion patterns:** `claudeMdExcludes` glob patterns skip files relative to project root

## Testing Coverage

**Total: 40+ test cases**

- Claude walk-up discovery (merge order, symlinks, excludes, imports)
- Codex walk-down discovery (project root detection, one-file-per-level, fallback chain)
- Integration (Materialize with discovery, explicit wins, platform defaults)
- Granular control (boolean parsing, category flags, common use cases)
- Security (symlink boundaries, traversal prevention, size limits, import depth)

## Implementation Status

- [x] Specification complete
- [ ] Phase 1: Core discovery engine (`pkg/materialize/discover.go`)
- [ ] Phase 2: Codex integration
- [ ] Phase 3: Claude advanced features (imports, rules, agents)
- [ ] Phase 4: Materialization integration & granular control parsing
- [ ] Phase 5: Documentation & release

## References

- **Claude Code CLI:** CLAUDE.md walk-up, settings.json cascade, .mcp.json merge, @import syntax
- **Codex CLI:** AGENTS.md walk-down, config.toml cascade with trust model, project root markers
- **PAOP Persistence:** Agent home pattern, credential sync, Docker networking patterns
- **Existing Materializer:** `/pkg/materialize/materializer.go` (lines 80-214 for integration points)
- **SessionRequest Schema:** `/pkg/api/schema/types.go` (lines 11-56 for API extensions)
