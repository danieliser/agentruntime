# Claude Code Hooks in agentruntime

Claude Code hooks are shell commands that execute in response to tool-use events during an agent session. This guide covers how to configure and use them in agentruntime.

## What are hooks?

Hooks are arbitrary shell commands that run at specific lifecycle points in Claude Code's execution:

- **PreToolUse** — before Claude invokes a tool
- **PostToolUse** — after a tool completes
- **Stop** — when the session ends
- Other event types supported by Claude Code

For example, you might use a hook to:
- Log tool invocations to a monitoring system
- Validate tool inputs before they execute
- Clean up resources after a tool completes
- Emit metrics at specific lifecycle points

Refer to the [Claude Code documentation](https://claude.ai/docs) for the full list of hook events and their semantics.

## Default behavior

By default, agentruntime initializes Claude Code sessions with an empty hooks configuration:

```json
{
  "hooks": {}
}
```

This means **no hooks execute** in agent sessions unless you explicitly configure them. This is the safe default: agent sessions run in a controlled environment and should not inherit personal hook scripts from the operator's workstation.

## Enabling hooks

Pass hooks via the `claude.settings_json.hooks` field in your `SessionRequest`.

### HTTP API

```json
{
  "agent": "claude",
  "prompt": "Your task here",
  "work_dir": "/path/to/project",
  "claude": {
    "settings_json": {
      "hooks": {
        "PreToolUse": [
          {
            "command": "echo 'About to use tool' >> /tmp/hooks.log",
            "match": "Bash"
          }
        ]
      }
    }
  }
}
```

### YAML (CLI)

```yaml
agent: claude
prompt: "Your task here"
work_dir: /path/to/project
claude:
  settings_json:
    hooks:
      PreToolUse:
        - command: "echo 'About to use tool' >> /tmp/hooks.log"
          match: "Bash"
```

### Go SDK

```go
req := &schema.SessionRequest{
  Agent:   "claude",
  Prompt:  "Your task here",
  WorkDir: "/path/to/project",
  Claude: &schema.ClaudeConfig{
    SettingsJSON: map[string]any{
      "hooks": map[string]any{
        "PreToolUse": []map[string]any{
          {
            "command": "echo 'About to use tool' >> /tmp/hooks.log",
            "match":   "Bash",
          },
        },
      },
    },
  },
}
```

## Hook structure

Each hook entry contains:

- **command** (string, required) — The shell command to execute
- **match** (string, optional) — Filter by tool type (e.g., `"Bash"`, `"Read"`, `"Write"`)

Other fields specific to Claude Code's hook format are supported and passed through as-is.

## Security considerations

Hooks run inside the agent session's execution environment:

- **Docker mode**: Hooks execute inside the container. They are sandboxed by container security boundaries (dropped capabilities, read-only mounts, network isolation).
- **Local mode**: Hooks execute on the host machine with the permissions of the agent process. Be cautious with untrusted hook configurations.

**Best practice**: Restrict hooks to diagnostic and monitoring commands. Avoid hooks that mutate critical files or systems outside the workspace.

## Codex

Codex does not support hooks. Hook configuration is a Claude Code feature and has no effect on Codex sessions.

## Tool permissions

Hooks are orthogonal to tool permissions. Permissions restrict which tools Claude can invoke; hooks observe when tools are invoked.

You can configure tool permissions via `claude.settings_json.permissions`:

```json
{
  "claude": {
    "settings_json": {
      "permissions": {
        "allow": ["Read", "Write", "Bash(ls:*)", "Bash(find:*)"],
        "deny": ["Bash(rm:*)", "Bash(sudo:*)"]
      },
      "hooks": {
        "PreToolUse": [...]
      }
    }
  }
}
```

Permissions are evaluated first (tool must be allowed to run). If allowed, hooks fire. Hooks cannot override permissions.

For the full permissions model, see the [Claude Code settings documentation](https://claude.ai/docs).
