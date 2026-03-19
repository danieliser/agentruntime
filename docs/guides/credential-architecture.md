# Credential Architecture

How agentruntime handles authentication for Claude Code and Codex agents.
This document is relevant for any orchestrator (PERSIST, custom schedulers, CI/CD)
that dispatches sessions to agentruntime.

## Core Principle

**Agents authenticate via OAuth credential files, never via API keys in environment variables.**

AI provider API keys (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`) override OAuth in both
Claude Code and Codex. When an API key is present, the agent uses API pricing (with
rate limits and usage caps) instead of the subscription. Agentruntime strips these
keys from the agent process environment to prevent this.

## Stripped Environment Variables

Both local and Docker runtimes remove these from the agent process:

| Variable | Reason |
|---|---|
| `ANTHROPIC_API_KEY` | Overrides Claude OAuth, hits API rate limits |
| `OPENAI_API_KEY` | Overrides Codex OAuth, hits API rate limits |
| `CODEX_API_KEY` | Same as OPENAI — Codex variant |

These are stripped regardless of how they enter the daemon process (shell env, .env
file, systemd unit). The stripping happens at the runtime layer, not the API layer.

## How Agents Actually Authenticate

### Claude Code

1. **Credential sync** (recommended): Start agentd with `--credential-sync`. The daemon
   extracts OAuth tokens from macOS Keychain (`Claude Code-credentials`) every 30 seconds
   and caches them at `{data_dir}/credentials/claude-credentials.json`.

2. **Materialization**: When a Claude session is created, the materializer discovers
   credentials in this priority order:
   - Explicit `claude.credentials_path` in the session request
   - Credential sync cache (`{data_dir}/credentials/claude-credentials.json`)
   - Host `~/.claude/.credentials.json`
   - Host `~/.claude/credentials.json`

3. **Mount**: The discovered credentials file is copied into the per-session config
   directory, which is mounted at `/home/agent/.claude/` in the container (or used
   directly in local mode via `XDG_CONFIG_HOME`).

### Codex

1. **Auth discovery**: The materializer looks for Codex OAuth auth.json in:
   - Credential sync cache (`{data_dir}/credentials/codex-auth.json`)
   - Host `~/.codex/auth.json`

2. **Mount**: The auth.json is copied into the per-session codex directory,
   mounted at `/home/agent/.codex/` in the container.

3. **No API key fallback**: Unlike the persistence project, agentruntime does NOT
   forward `OPENAI_API_KEY` as a fallback. Codex must have a valid `auth.json`.

## What Orchestrators Must NOT Do

1. **Do not set `ANTHROPIC_API_KEY` in `req.Env`** — it will override OAuth inside
   the agent and hit your API usage cap.

2. **Do not set `OPENAI_API_KEY` in `req.Env`** — same problem for Codex.

3. **Do not rely on the daemon's environment for credentials** — the runtime strips
   AI keys from the process tree. Even if the daemon has `ANTHROPIC_API_KEY` set,
   agent sessions will not see it.

## What Orchestrators Should Do

1. **Ensure OAuth is set up**: Run `claude setup-token` (for Claude) or `codex login`
   (for Codex) on the host. The daemon's `--credential-sync` keeps these fresh.

2. **Pass `GH_TOKEN` via `req.Env` if needed**: GitHub tokens are NOT stripped — they're
   needed for git operations inside the agent. Pass them explicitly:

   ```json
   {
     "agent": "claude",
     "prompt": "...",
     "env": {
       "GH_TOKEN": "ghp_..."
     }
   }
   ```

3. **Use `--credential-sync` on the daemon**: This is the simplest path. It handles
   Keychain extraction, token refresh, and file caching automatically.

## Diagnosing Auth Failures

If an agent session fails with `401 Unauthorized` or API usage limit errors:

1. Check the session log for `apiKeySource`:
   ```bash
   curl http://localhost:8090/sessions/{id}/log | grep apiKeySource
   ```
   - `"apiKeySource":"ANTHROPIC_API_KEY"` → API key is leaking through. Check the daemon's environment.
   - `"apiKeySource":"oauth"` → OAuth is working. Check token expiry.

2. Check credential sync is running:
   ```bash
   agentd --credential-sync ...
   ```

3. Check credential files exist:
   ```bash
   ls -la ~/.local/share/agentruntime/credentials/
   ls -la ~/.claude/.credentials.json
   ls -la ~/.codex/auth.json
   ```

4. Force credential refresh:
   ```bash
   claude setup-token   # Claude Code
   codex login          # Codex
   ```

## PERSIST-Specific Guidance

PERSIST's `agentruntime_runtime.py` dispatches sessions to agentruntime. Key points:

- **Do not forward `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` in `context["env"]`** —
  agentruntime handles credential materialization.
- **Include `"claude": {}` or `"codex": {}` in the request** — even an empty block
  triggers credential materialization. Without it, no credentials are mounted.
  (agentruntime now auto-infers this from `agent` name, but explicit is better.)
- **PERSIST's own daemon sessions** (not through agentruntime) still inherit the host
  environment. If `ANTHROPIC_API_KEY` is set in PERSIST's environment, those sessions
  will use API pricing. Consider stripping it from PERSIST's own tmux session env too.
