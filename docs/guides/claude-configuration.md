# Claude Code Configuration Guide

This guide documents the Claude Code agent configuration for agentruntime. When you spawn a Claude Code session, agentruntime materializes configuration files into the agent's home directory, manages credentials, and passes CLI flags to control behavior.

## Configuration Fields

### `settings_json`

**JSON key:** `settings_json`
**Materialized to:** `~/.claude/settings.json`
**Type:** Object (map[string]any)

Claude's user settings file. Pass structured JSON config that Claude Code reads at startup. The field is optional; if omitted, a minimal settings file is created.

**Special handling:** agentruntime automatically sets `skipDangerousModePermissionPrompt: true` in the settings file. This pre-accepts the dangerous mode permission dialog so Claude doesn't display an interactive TUI prompt when `--dangerously-skip-permissions` is used.

**Example:**

```json
{
  "claude": {
    "settings_json": {
      "autoSave": true,
      "theme": "dark",
      "codeEditorFontSize": 13
    }
  }
}
```

### `claude_md`

**JSON key:** `claude_md`
**Materialized to:** `~/.claude/CLAUDE.md`
**Type:** String (markdown)

Global instructions for Claude Code. This file is read when Claude starts and influences its behavior across all sessions in the container. Use this to communicate project-specific workflows, coding standards, or behavioral preferences.

**Example:**

```json
{
  "claude": {
    "claude_md": "# Project Guidelines\n\n- Use TypeScript for all new code\n- Follow ESLint rules in .eslintrc.json\n- Test coverage must be > 80%"
  }
}
```

### `mcp_json`

**JSON key:** `mcp_json`
**Materialized to:** `~/.claude/.mcp.json`
**Type:** Object (map[string]any)

MCP (Model Context Protocol) server configuration. This field is *merged* with any MCP servers from the top-level `mcp_servers` array in the request.

The merged configuration is written to `~/.claude/.mcp.json` in standard Claude MCP format:

```json
{
  "mcpServers": {
    "server-name": {
      "type": "stdio" | "http" | "websocket",
      "url": "...",
      "cmd": [...],
      "env": {...}
    }
  }
}
```

**Example:**

```json
{
  "claude": {
    "mcp_json": {
      "mcpServers": {
        "custom-local-server": {
          "type": "stdio",
          "cmd": ["/usr/local/bin/my-mcp-server"],
          "env": {"DEBUG": "1"}
        }
      }
    }
  }
}
```

### `credentials_path`

**JSON key:** `credentials_path`
**Mounted at:** `~/.claude/credentials.json` (read-only bind mount)
**Type:** String (file path)

Path to Claude's credentials file on the host machine. If provided, agentruntime bind-mounts this file into the container as read-only.

Supports `~` and environment variable expansion (e.g., `$HOME`, `$CLAUDE_HOME`).

**Credential discovery chain** (when `credentials_path` is not provided):

1. **Credential sync cache** (populated by daemon `--credential-sync` flag):
   `{daemon-data-dir}/credentials/claude-credentials.json`

2. **Host `~/.claude/.credentials.json`**

3. **Host `~/.claude/credentials.json`**

If no credentials are found, Claude will attempt to authenticate interactively.

**Example:**

```json
{
  "claude": {
    "credentials_path": "~/.claude/.credentials.json"
  }
}
```

### `memory_path`

**JSON key:** `memory_path`
**Mounted at:** `~/.claude/projects/{hash}/` (read-only bind mount)
**Type:** String (file path)

Path to a local directory containing Claude's project memory files. This enables Claude to maintain persistent context across sessions.

The directory is mounted read-only. The mount path is derived from a SHA256 hash of the full expanded host path (first 16 hex chars), ensuring stable paths for the same directory across invocations.

Supports `~` and environment variable expansion.

**Example:**

```json
{
  "claude": {
    "memory_path": "~/.claude/memory/my-project"
  }
}
```

### `max_turns`

**JSON key:** `max_turns`
**CLI equivalent:** `--max-turns N`
**Type:** Integer

Limits the number of agentic turns (reasoning loops) Claude can take. Each turn is one iteration of Claude analyzing, deciding on tools, and acting.

If set to 0 or omitted, no limit is enforced (Claude runs until it reaches a natural stopping point or timeout).

**Example:**

```json
{
  "claude": {
    "max_turns": 5
  }
}
```

### `allowed_tools`

**JSON key:** `allowed_tools`
**CLI equivalent:** `--allowedTools tool1 --allowedTools tool2`
**Type:** Array of strings

Restrict which tools Claude can use. This whitelist applies on top of any tool configuration in `mcp_json` or `mcp_servers`.

Omitting this field allows all configured tools. Providing an empty array disables all tools.

**Common tool names:** `read_file`, `write_file`, `list_directory`, `bash`, `git`, etc. (depends on your MCP server configuration).

**Example:**

```json
{
  "claude": {
    "allowed_tools": ["read_file", "list_directory", "bash"]
  }
}
```

### `output_format`

**JSON key:** `output_format`
**Type:** String
**Status:** Ignored

This field is accepted for backward compatibility but has no effect. The sidecar always uses `stream-json` format for structured event streaming to the daemon, regardless of what you specify here.

## Default Configuration Inference

When you send `{"agent": "claude"}` without a `claude` configuration block, the daemon automatically infers `"claude": {}` — an empty ClaudeConfig. agentruntime then materializes a minimal valid setup:

- Empty `settings.json` (with `skipDangerousModePermissionPrompt: true` auto-added)
- Empty `CLAUDE.md`
- Empty `.mcp.json`
- Credentials discovered via the standard chain
- No memory path mounted
- No max_turns or allowed_tools restrictions

This lets you run Claude with zero configuration:

```json
{
  "agent": "claude",
  "prompt": "Build a hello-world web server"
}
```

## Auto-Trust Mechanism

agentruntime generates a `.claude.json` state file that pre-trusts the `/workspace` directory. This prevents Claude from displaying an interactive trust dialog when it starts.

The file is created in the session directory and bind-mounted as `~/.claude.json` (read-write). It includes:

- `projects["/workspace"].hasTrustDialogAccepted = true`
- `hasCompletedOnboarding = true`
- Other default startup state to skip onboarding screens

This happens automatically; no configuration is needed.

## Materialization

When you create a session with Claude config, here's what agentruntime does:

1. **Resolve paths:** Expand `~`, `$VAR`, and relative paths in `credentials_path` and `memory_path`.

2. **Discover credentials:** If `credentials_path` is not set, search the credential sync cache, then `~/.claude/.credentials.json`, then `~/.claude/credentials.json` on the host.

3. **Create session directory:** If using persistent session storage (daemon with `--data-dir`), create a per-session subdirectory under `{data-dir}/claude/{session-id}/`. Otherwise, create a temporary directory.

4. **Write files:**
   - `settings.json` (inline content + auto-added `skipDangerousModePermissionPrompt`)
   - `CLAUDE.md` (inline content)
   - `.mcp.json` (merged from `mcp_json` + `mcp_servers`)
   - `.claude.json` (state file for auto-trust)

5. **Create mounts:** Bind-mount the session directory at `~/.claude` (read-write), credentials at `~/.claude/credentials.json` (read-only), memory at `~/.claude/projects/{hash}/` (read-only), and state file at `~/.claude.json` (read-write).

6. **Pass CLI flags:** Build the `claude` command with:
   - `--max-turns N` (if `max_turns > 0`)
   - `--allowedTools tool1 --allowedTools tool2` (if `allowed_tools` is set)
   - Standard flags: `--dangerously-skip-permissions`, `--output-format stream-json`, `--verbose`

## Examples

### Minimal Configuration

The simplest valid request — no config block needed:

```json
{
  "agent": "claude",
  "prompt": "Write a simple Python script that prints hello world"
}
```

### With Settings Override

Override Claude's default settings:

```json
{
  "agent": "claude",
  "prompt": "Refactor the auth module",
  "claude": {
    "settings_json": {
      "autoSave": true,
      "theme": "light"
    }
  }
}
```

### With MCP Servers

Inject MCP servers so Claude can use custom tools:

```json
{
  "agent": "claude",
  "prompt": "Query the database and generate a report",
  "mcp_servers": [
    {
      "name": "db-connector",
      "type": "stdio",
      "cmd": ["/opt/db-mcp", "--port", "5432"]
    }
  ],
  "claude": {
    "allowed_tools": ["read_file", "bash", "db-connector-query"]
  }
}
```

### With Project Memory

Persist context across sessions:

```json
{
  "agent": "claude",
  "prompt": "Continue the implementation from the last session",
  "claude": {
    "memory_path": "~/.claude/memory/myproject"
  }
}
```

### With Max Turns and Constraints

Limit reasoning depth and tool usage:

```json
{
  "agent": "claude",
  "prompt": "Fix the login bug and write tests",
  "claude": {
    "max_turns": 10,
    "allowed_tools": ["read_file", "write_file", "bash"],
    "claude_md": "# Constraints\n\n- Only modify files in src/auth/\n- Run tests after each change\n- Report back when done"
  }
}
```

### With Custom Instructions and Credentials

Fully customized environment with explicit credentials:

```json
{
  "agent": "claude",
  "prompt": "Deploy the service to production",
  "claude": {
    "claude_md": "# Deployment Runbook\n\n1. Check CI status\n2. Run E2E tests\n3. Tag release\n4. Push to registry\n5. Update helm values\n6. Wait for rollout",
    "credentials_path": "/secure/claude/prod-credentials.json",
    "max_turns": 15,
    "allowed_tools": ["read_file", "bash", "git"],
    "settings_json": {
      "autoSave": true
    }
  }
}
```

## CLI Passthrough and Legacy Fields

The sidecar accepts additional configuration via the `AGENT_CONFIG` environment variable:

- `model` — Override the Claude model (e.g., `"claude-opus-4-5"`)
- `max_turns` — Also accepted here; merged with the ClaudeConfig field
- `allowed_tools` — Also accepted here; merged with the ClaudeConfig field
- `effort` — Set reasoning effort (`"low"`, `"medium"`, `"high"`)
- `resume_session` — Session ID to resume instead of starting fresh

These are normally set by the daemon's request handler and do not require manual configuration.

## Environment Variables

agentruntime passes a sanitized environment to Claude Code:

- **Inherited:** `PATH`, `HOME`, `USER`, `LANG`, `TERM`, `SHELL`, `TMPDIR`, `NODE_OPTIONS`
- **Auth:** `CLAUDE_CODE_OAUTH_TOKEN` (from credential sync), `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`
- **Proxy:** `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` (set by Docker network manager)

Host environment variables do not leak into the container. Only the variables listed above and any explicitly set in the request are passed through.

## Troubleshooting

**Claude shows a trust dialog:**
- agentruntime auto-generates `.claude.json` with trust pre-set; if this fails, ensure the session directory is writable.

**Credentials not found:**
- Check the credential discovery chain: sync cache → `~/.claude/.credentials.json` → `~/.claude/credentials.json`.
- Use explicit `credentials_path` to bypass discovery.

**Tools not available to Claude:**
- Verify MCP servers are in `mcp_servers` at the top level.
- Check `allowed_tools` doesn't whitelist too restrictively.
- Review `mcp_json` for correct server configuration.

**Settings not applied:**
- Ensure `settings_json` is a valid JSON object (not a string).
- Remember that `skipDangerousModePermissionPrompt` is auto-added — don't override it.

**Memory path not mounted:**
- Expand `memory_path` manually (`~/` becomes the host home, `$VAR` is resolved).
- Ensure the directory exists on the host.
- Check session directory mount permissions (read-write) and memory mount (read-only).
