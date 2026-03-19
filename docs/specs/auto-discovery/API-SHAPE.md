# SessionRequest AutoDiscover API Shape

Quick reference for the three forms of auto-discovery control in agentruntime.

## Form 1: Boolean Shorthand

```go
// Enable all discovery (full context)
type SessionRequest struct {
    AutoDiscover bool // true = all categories enabled
}

// Disable all discovery (explicit config only)
type SessionRequest struct {
    AutoDiscover bool // false = no discovery
}

// Unset (platform default)
type SessionRequest struct {
    AutoDiscover interface{} // nil/null → Docker: true, Local: true
}
```

### JSON Examples

```json
// Full discovery
{
  "agent": "claude",
  "auto_discover": true,
  "work_dir": "/path/to/project",
  "prompt": "Help with this..."
}

// No discovery
{
  "agent": "claude",
  "auto_discover": false,
  "claude": {
    "claude_md": "# Custom instructions only"
  },
  "prompt": "Help with this..."
}

// Default (unset)
{
  "agent": "claude",
  "work_dir": "/path/to/project",
  "prompt": "Help with this..."
}
```

## Form 2: Granular Control (Map)

```go
// Whitelist specific categories to discover
type SessionRequest struct {
    AutoDiscover map[string]bool // {"claude_md": true, "settings": false, ...}
}
```

**Category Keys:**

Claude:
- `claude_md` — CLAUDE.md files (walk-up, all collected)
- `settings` — settings.json cascade (4-level merge)
- `mcp` — .mcp.json merge and user ~/.claude.json servers
- `rules` — .claude/rules/ recursive discovery
- `agents` — .claude/agents/ directory discovery

Codex:
- `agents_md` — AGENTS.md files (walk-down, concatenated)
- `config_toml` — config.toml cascade (5-level merge with trust check)

**All unspecified keys default to false** (whitelist/opt-in).

### JSON Examples

```json
// Instructions only (context without settings/tools)
{
  "agent": "claude",
  "auto_discover": {
    "claude_md": true
  },
  "work_dir": "/path/to/project",
  "prompt": "Help with this..."
}
```

```json
// Everything except MCP (avoid pulling host tool registrations)
{
  "agent": "claude",
  "auto_discover": {
    "claude_md": true,
    "settings": true,
    "rules": true,
    "agents": true,
    "mcp": false
  },
  "work_dir": "/path/to/project",
  "prompt": "Help with this..."
}
```

```json
// Codex: context without config (trust boundary)
{
  "agent": "codex",
  "auto_discover": {
    "agents_md": true,
    "config_toml": false
  },
  "work_dir": "/path/to/project",
  "prompt": "Help with this..."
}
```

```json
// Only rules (no CLAUDE.md, no settings)
{
  "agent": "claude",
  "auto_discover": {
    "rules": true
  },
  "work_dir": "/path/to/project",
  "prompt": "Help with this..."
}
```

## Form 3: Type Definition

```go
type SessionRequest struct {
    // ...existing fields...

    // AutoDiscover controls configuration auto-discovery from the filesystem.
    // Supports three forms:
    //
    // 1. bool: true/false/nil (shorthand)
    //    true  → enable all discovery
    //    false → disable all discovery
    //    nil   → platform default (Docker: true, Local: true)
    //
    // 2. map[string]bool: {"claude_md": true, "settings": false, ...}
    //    All unspecified keys default to false (whitelist/opt-in)
    //    Valid keys: claude_md, settings, mcp, rules, agents (Claude)
    //               agents_md, config_toml (Codex)
    //
    // Explicit SessionRequest.Claude/Codex fields always win.
    AutoDiscover interface{} `json:"auto_discover,omitempty" yaml:"auto_discover,omitempty"`
}
```

## Precedence Rules

1. Explicit SessionRequest fields ALWAYS take priority
2. If field is empty/null and category is enabled, discovered value is used
3. For composite fields (settings.json, config.toml, .mcp.json):
   - Discovered fills gaps
   - Explicit values win on conflicts
   - Arrays merge with dedup

```go
// Pseudocode
if categories["claude_md"] && req.Claude.ClaudeMD == "" {
    req.Claude.ClaudeMD = discovered.ClaudeMD
}

if categories["settings"] && req.Claude.SettingsJSON == nil {
    req.Claude.SettingsJSON = discovered.SettingsJSON
} else if categories["settings"] && req.Claude.SettingsJSON != nil {
    req.Claude.SettingsJSON = deepMerge(discovered, explicit)
}
```

## Parsing Logic

```go
func parseAutoDiscover(raw interface{}) map[string]bool {
    categories := make(map[string]bool)

    switch v := raw.(type) {
    case nil:
        // Platform default (handled by caller)
        return nil

    case bool:
        // Shorthand: expand to all categories
        val := v
        return map[string]bool{
            "claude_md":   val,
            "settings":    val,
            "mcp":         val,
            "rules":       val,
            "agents":      val,
            "agents_md":   val,
            "config_toml": val,
        }

    case map[string]interface{}:
        // Granular: whitelist (all unspecified = false)
        for key := range v {
            if bval, ok := v[key].(bool); ok {
                categories[key] = bval
            }
        }
        return categories

    default:
        // Invalid: treat as nil (platform default)
        return nil
    }
}
```

## Common Use Cases

| Use Case | Value |
|----------|-------|
| Full discovery (default) | `true` |
| No discovery | `false` |
| Context only | `{"claude_md": true}` |
| Context + settings | `{"claude_md": true, "settings": true}` |
| Everything except MCP | `{"claude_md": true, "settings": true, "rules": true, "agents": true, "mcp": false}` |
| Codex context only | `{"agents_md": true}` |
| Codex context + config | `{"agents_md": true, "config_toml": true}` |

## Defaults by Runtime

- **Docker:** `auto_discover: true` (if unset)
- **Local:** `auto_discover: true` (if unset)
- **Explicit:** Caller can override with any form
