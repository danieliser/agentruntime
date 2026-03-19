# Codex Configuration Guide

This guide covers configuring the Codex agent in agentruntime. Codex is OpenAI's autonomous coding agent, designed for agentic code execution with structured reasoning.

## Configuration Structure

Codex configuration is provided in the session request's `codex` block:

```json
{
  "agent": "codex",
  "codex": {
    "config_toml": { "api_key": "..." },
    "instructions": "...",
    "approval_mode": "full-auto"
  }
}
```

If you specify `"agent": "codex"` without a `codex` block, agentruntime automatically creates an empty config object `{}`, triggering default configuration inference.

## CodexConfig Fields

### `config_toml` — TOML Configuration

**JSON key:** `config_toml`
**Type:** object (map[string]any)
**Materialized to:** `~/.codex/config.toml`

The `config_toml` field contains key-value pairs that are serialized into Codex's TOML configuration file. This includes model settings, API keys, and other Codex options.

**Default behavior:**
- If omitted or null, agentruntime creates an empty TOML file.
- Agentruntime automatically appends two defaults that Codex expects:
  - `model_reasoning_effort = "high"`
  - `[projects."/workspace"]` section with `trust_level = "trusted"`

**Example:**

```json
{
  "codex": {
    "config_toml": {
      "api_key": "sk-...",
      "model": "gpt-4o"
    }
  }
}
```

This generates `~/.codex/config.toml`:

```toml
api_key = "sk-..."
model = "gpt-4o"

# agentruntime defaults
model_reasoning_effort = "high"

[projects."/workspace"]
trust_level = "trusted"
```

**Note:** The `config_toml` field cannot represent deeply nested TOML sections. For simple key-value pairs and flat tables, use this field. Complex nested structures should be managed through instructions or environment variables passed via `AGENT_CONFIG.Env`.

### `instructions` — Custom Instructions

**JSON key:** `instructions`
**Type:** string
**Materialized to:** `~/.codex/instructions.md`

Custom instructions for Codex to follow during execution. These are user-facing guidelines that shape the agent's behavior without affecting the core reasoning engine.

**Example:**

```json
{
  "codex": {
    "instructions": "Always use TypeScript. Run tests after changes. Keep functions under 50 lines."
  }
}
```

This creates `~/.codex/instructions.md` with the provided content.

### `approval_mode` — Tool Approval Policy

**JSON key:** `approval_mode`
**Type:** string
**Valid values:** `"full-auto"` | `"auto-edit"` | `"suggest"`
**Default:** `"full-auto"`

Controls how Codex requests approval for tool use (file changes, command execution, etc.).

| Mode | Behavior |
|------|----------|
| `"full-auto"` | Execute all tools without asking. Fastest for hands-off sessions. |
| `"auto-edit"` | Auto-apply changes; ask for approval only if execution fails. |
| `"suggest"` | Always ask for approval before executing tools. |

**Example:**

```json
{
  "codex": {
    "approval_mode": "suggest"
  }
}
```

## Authentication

Codex requires OpenAI API credentials to function. Agentruntime discovers authentication in this priority order:

1. **Credential sync cache:** `{dataDir}/credentials/codex-auth.json`
   - Populated by the daemon's `--credential-sync` flag, which pulls credentials from host keychain/file sources.
   - Enables secure credential passing without embedding tokens in session requests.

2. **Host default location:** `~/.codex/auth.json`
   - Fallback to the user's default Codex authentication file.
   - Used only if the sync cache is unavailable.

**Important:** The `OPENAI_API_KEY` environment variable is **not** automatically forwarded to Codex containers. Use `config_toml` or the credential sync mechanism instead.

**Credential sync example:**

```bash
# Daemon running with credential sync enabled
./agentd --credential-sync keychain --port 8090
```

Agentruntime will automatically use synced credentials if available in the data directory.

## Model Override

Override Codex's default model via the `AGENT_CONFIG` environment variable (set by the daemon):

```json
{
  "model": "gpt-4o"
}
```

This adds `--model gpt-4o` to the Codex command line, overriding `config_toml` settings.

## Examples

### Minimal Configuration

Start Codex with defaults (no custom instructions, full-auto approval):

```json
{
  "agent": "codex"
}
```

### Custom Instructions with Default Model

Provide task-specific guidance:

```json
{
  "agent": "codex",
  "codex": {
    "instructions": "Focus on Python. Add type hints to all functions. Use pytest for tests."
  }
}
```

### Model Override with API Key

Use a specific model and inline credentials:

```json
{
  "agent": "codex",
  "codex": {
    "config_toml": {
      "api_key": "sk-proj-..."
    }
  },
  "agentConfig": {
    "model": "gpt-4-turbo"
  }
}
```

### Approval Mode for Interactive Sessions

Ask before executing tools:

```json
{
  "agent": "codex",
  "codex": {
    "approval_mode": "suggest",
    "instructions": "You are assisting a developer. Ask for confirmation before making changes."
  }
}
```

### Credential Sync (Host Setup)

When the daemon is running with credential sync:

```bash
./agentd --credential-sync file:~/.credentials.json --port 8090
```

The Codex session automatically discovers credentials from the sync cache. No explicit `config_toml` needed:

```json
{
  "agent": "codex",
  "codex": {
    "instructions": "Code changes only, no tests."
  }
}
```

## Workspace Trust

Agentruntime automatically trusts the `/workspace` directory (the mounted project root) by adding:

```toml
[projects."/workspace"]
trust_level = "trusted"
```

This prevents Codex from prompting for permission to modify workspace files. The `trust_level` can be overridden in `config_toml` if needed:

```json
{
  "codex": {
    "config_toml": {
      "projects": {
        "/workspace": {
          "trust_level": "trusted"
        }
      }
    }
  }
}
```

However, since the materializer supports only flat TOML tables, complex nested structures like this should be managed directly in the host's `~/.codex/config.toml` and referenced via credential/config file paths.

## Environment Variables

Additional environment variables can be passed to the Codex process via `AGENT_CONFIG.Env`:

```json
{
  "agentConfig": {
    "env": {
      "OPENAI_ORG_ID": "org-xxx",
      "CODEX_VERBOSE": "1"
    }
  }
}
```

These are merged into the Codex process environment, layered on top of agentruntime's clean base environment.

## Summary

| Field | Use Case |
|-------|----------|
| `config_toml` | API keys, model override, Codex-specific settings. |
| `instructions` | Custom behavior guidelines for the session. |
| `approval_mode` | Control tool approval prompts (`full-auto`, `auto-edit`, `suggest`). |

Agentruntime materializes these into `~/.codex/` and injects workspace trust defaults. Authentication is discovered automatically from credential sync cache or host defaults. For complex configurations, manage settings in the host's `~/.codex/config.toml` and rely on the daemon's credential sync to pass authentication securely.
