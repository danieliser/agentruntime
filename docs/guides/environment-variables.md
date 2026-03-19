# Environment Variables Reference

Environment variables configure agentruntime across three layers: daemon startup, sidecar behavior, and container runtime. This guide documents all supported variables, their defaults, and usage context.

## Daemon (`agentd`)

Variables that control the `agentd` daemon process at startup.

### `AGENTRUNTIME_DATA_DIR`

**Type:** Path
**Default:** `$XDG_DATA_HOME/agentruntime` or `~/.local/share/agentruntime`
**Context:** daemon startup

Root directory for session logs, credentials, and materialized configs. Takes precedence over `XDG_DATA_HOME`.

```bash
export AGENTRUNTIME_DATA_DIR=/var/lib/agentruntime
./agentd
```

### `XDG_DATA_HOME`

**Type:** Path
**Default:** `~/.local/share`
**Context:** daemon startup (fallback)

XDG Base Directory standard for data storage. Used as fallback if `AGENTRUNTIME_DATA_DIR` is not set. agentruntime appends `/agentruntime` to this path.

```bash
export XDG_DATA_HOME=/data
# Results in: /data/agentruntime
```

### `DOCKER_HOST`

**Type:** URL
**Default:** Empty (use local Docker daemon)
**Context:** daemon startup, passed through to all docker commands
**Flag override:** `--docker-host`

Remote Docker daemon address. Supports `ssh://` and `tcp://` schemes. When set via flag (`-docker-host`), all docker CLI commands run with `DOCKER_HOST` in their environment.

```bash
./agentd --runtime docker --docker-host "ssh://deploy@prod-1"
# All docker commands inside the daemon will use the remote host
```

**Note:** This is distinct from running agentd itself over SSH. The daemon runs locally but dispatches docker commands to the specified host.

## Sidecar (`agentruntime-sidecar`)

Variables that control sidecar startup and agent backend behavior inside the container.

### `AGENT_CMD` (Required)

**Type:** JSON array
**Default:** None (required)
**Context:** sidecar startup

Command and arguments to launch the agent binary. Must be valid JSON representing an array of strings. The first element is the binary path; remaining elements are arguments.

```bash
# Claude Code
export AGENT_CMD='["claude"]'

# Codex with arguments
export AGENT_CMD='["codex", "--no-history"]'

# Custom agent
export AGENT_CMD='["python", "/usr/local/bin/myagent.py"]'
```

If not set, the sidecar looks for legacy `AGENT_BIN` / `AGENT_ARGS_JSON` and fails if neither are present.

### `AGENT_CONFIG`

**Type:** JSON object
**Default:** Empty (optional)
**Context:** sidecar startup

Agent-specific configuration serialized as JSON. Normally set by the daemon when materializing a session request. Contains model, session resume info, and env vars for the agent process.

```bash
export AGENT_CONFIG='{"model":"claude-opus-4-5","max_turns":10,"env":{"DEBUG":"1"}}'
```

**Schema:**

```json
{
  "model": "string (optional)",
  "resume_session": "string (optional)",
  "env": { "VAR": "value" },
  "approval_mode": "string (optional, Codex only)",
  "max_turns": 10,
  "allowed_tools": ["string"]
}
```

See [cmd/sidecar/agentconfig.go](../../cmd/sidecar/agentconfig.go) for full details.

### `AGENT_PROMPT`

**Type:** String
**Default:** Empty (interactive mode)
**Context:** sidecar startup

Initial prompt to send to the agent. When set, the sidecar enters fire-and-fire mode (one-shot prompt), runs the agent to completion, and exits. When empty, the sidecar runs in interactive mode, accepting commands over WebSocket.

```bash
# One-shot mode
export AGENT_PROMPT="List all files in /workspace"

# Interactive mode (default when unset)
unset AGENT_PROMPT
```

### `SIDECAR_PORT`

**Type:** Port number (1-65535)
**Default:** `9090`
**Context:** sidecar startup

HTTP server port for the sidecar's WebSocket bridge. Must be numeric and valid.

```bash
export SIDECAR_PORT=8888
```

### `SIDECAR_CLEANUP_TIMEOUT`

**Type:** Duration string or seconds
**Default:** `60` (seconds)
**Context:** sidecar startup

Inactivity timeout before the sidecar automatically shuts down. Accepts integer seconds or duration strings (e.g., `30s`, `2m`, `1h30m`).

```bash
# Seconds
export SIDECAR_CLEANUP_TIMEOUT=120

# Duration string
export SIDECAR_CLEANUP_TIMEOUT=2m30s

# Disable (0)
export SIDECAR_CLEANUP_TIMEOUT=0
```

Must be non-negative. Used for resource cleanup when idle containers are no longer needed.

### `AGENT_BIN` (Legacy)

**Type:** Path
**Default:** Empty
**Context:** sidecar startup (fallback for v1 PTY bridge)

Deprecated. Use `AGENT_CMD` instead. If `AGENT_CMD` is not set and `AGENT_BIN` is set, the sidecar falls back to the older PTY-based bridge.

```bash
# Legacy (do not use)
export AGENT_BIN="/usr/local/bin/claude"
export AGENT_ARGS_JSON='["--model","claude-opus-4-5"]'
```

### `AGENT_ARGS_JSON` (Legacy)

**Type:** JSON array
**Default:** Empty
**Context:** sidecar startup (fallback for v1 PTY bridge)

Deprecated. Arguments to pass to `AGENT_BIN` as JSON. If this fails to parse as JSON, falls back to space-splitting `AGENT_ARGS`.

### `AGENT_ARGS` (Legacy)

**Type:** String
**Default:** Empty
**Context:** sidecar startup (fallback for v1 PTY bridge)

Deprecated. Space-separated arguments to `AGENT_BIN`. Only used if both `AGENT_ARGS_JSON` is not JSON and `AGENT_CMD` is not set.

## Container Proxy

Variables that control egress from Docker containers via the managed proxy sidecar.

These are **automatically injected** into agent containers by the Docker runtime and should not be manually set unless running in an isolated environment.

### `HTTP_PROXY`

**Type:** URL
**Default:** `http://agentruntime-proxy:3128`
**Context:** auto-injected into Docker containers

HTTP proxy for outbound connections. Containers use the Squid-based `agentruntime-proxy` sidecar on the managed `agentruntime-agents` network.

### `HTTPS_PROXY`

**Type:** URL
**Default:** `http://agentruntime-proxy:3128`
**Context:** auto-injected into Docker containers

HTTPS proxy for outbound connections (note: proxy URL remains HTTP, but proxies HTTPS traffic).

### `NO_PROXY`

**Type:** Comma-separated list
**Default:** `localhost,127.0.0.1,host.docker.internal,host-gateway`
**Context:** auto-injected into Docker containers

Addresses that bypass the proxy. Includes loopback, Docker's internal host references, and the host gateway.

## Session Environment (`req.Env`)

Variables passed via the SessionRequest `Env` field are forwarded to the agent process. This is a **clean-room environment**: only variables explicitly provided are passed — the host environment is **not** inherited in Docker containers.

```json
{
  "env": {
    "DEBUG": "1",
    "MY_API_KEY": "secret",
    "WORKSPACE_ROOT": "/workspace"
  }
}
```

These variables are merged into the sidecar's `AGENT_CONFIG` and injected into the agent process (e.g., `claude` or `codex` subprocess).

### Reserved Keys (Blocked)

The following environment variables **cannot** be overridden via `req.Env`:

- `PATH` — agent sandbox cannot modify shell path search
- `LD_LIBRARY_PATH` — prevents library injection attacks
- `LD_PRELOAD` — prevents code injection via preloaded libraries
- `DYLD_LIBRARY_PATH` — macOS equivalent
- `DYLD_INSERT_LIBRARIES` — macOS equivalent
- `DYLD_FRAMEWORK_PATH` — macOS framework injection prevention

Attempting to set these will return a validation error.

```bash
# This will fail
{
  "env": {
    "PATH": "/custom/path"  # Error: PATH is reserved
  }
}
```

## Credential Environment

Certain environment variables carry sensitive authentication tokens or API keys. agentruntime treats these specially.

### Forwarding Behavior

**Forwarded to agent containers (via `req.Env`):**

These credentials are passed through to the agent process if explicitly included in the session request:

- `OPENAI_API_KEY` — OpenAI API key for Codex
- `GH_TOKEN` — GitHub token (CLI convention)
- `GITHUB_TOKEN` — GitHub token (Actions convention)
- `CLAUDE_CODE_OAUTH_TOKEN` — Claude Code OAuth session token

**Why forwarding is safe:** The agent container is already network-isolated via the managed proxy, and credentials are only injected if the session request explicitly includes them.

### Host Isolation

The following are **not** automatically inherited from the host:

- Agent containers receive only the variables in `req.Env`
- No host environment variables leak into containers
- Credentials stored in `$AGENTRUNTIME_DATA_DIR/credentials.json` are **not** automatically injected

To use host credentials, explicitly retrieve them and pass via `req.Env` (or use the daemon's optional credential sync feature with the `--credential-sync` flag).

## Container Networking

### `AGENTRUNTIME_PORT`

**Type:** Port number
**Default:** `8090` (daemon port)
**Context:** Linux iptables rules (Docker runtime)

Port used for iptables firewall rules on Linux hosts. Defines the allowed egress port from agent containers to the daemon. On non-Linux systems (macOS), this is a no-op since Docker Desktop handles isolation.

When set, the Docker runtime applies an iptables rule to the `br-agentruntime` bridge:

```bash
sudo iptables -I DOCKER-USER -i br-agentruntime ! -o br-agentruntime -p tcp ! --dport $AGENTRUNTIME_PORT -j DROP
```

This prevents inter-container lateral movement while allowing agent containers to reach the daemon.

```bash
export AGENTRUNTIME_PORT=9090
./agentd --runtime docker
```

**Note:** Requires iptables access (usually root). If the rule fails to apply, agentd logs a warning but continues (best-effort enforcement).

## Summary Table

| Variable | Layer | Type | Default | Required? |
|----------|-------|------|---------|-----------|
| `AGENTRUNTIME_DATA_DIR` | daemon | path | `~/.local/share/agentruntime` | No |
| `XDG_DATA_HOME` | daemon | path | `~/.local/share` | No (fallback) |
| `DOCKER_HOST` | daemon | URL | empty | No |
| `AGENT_CMD` | sidecar | JSON array | empty | Yes |
| `AGENT_CONFIG` | sidecar | JSON object | empty | No |
| `AGENT_PROMPT` | sidecar | string | empty | No |
| `SIDECAR_PORT` | sidecar | port | `9090` | No |
| `SIDECAR_CLEANUP_TIMEOUT` | sidecar | duration | `60s` | No |
| `HTTP_PROXY` | container | URL | injected | No (auto) |
| `HTTPS_PROXY` | container | URL | injected | No (auto) |
| `NO_PROXY` | container | list | injected | No (auto) |
| `AGENTRUNTIME_PORT` | network | port | `8090` | No |
| `*` (session env) | container | string | from `req.Env` | No |

## Examples

### Starting the daemon with custom data directory and remote Docker

```bash
export AGENTRUNTIME_DATA_DIR=/data/agentruntime
./agentd \
  --port 8090 \
  --runtime docker \
  --docker-host "ssh://deploy@prod.example.com"
```

### Running a sidecar interactively (local testing)

```bash
export AGENT_CMD='["claude"]'
export SIDECAR_PORT=9090
./agentruntime-sidecar
# Sidecar listens on port 9090, waiting for WebSocket commands
```

### Running a sidecar in one-shot mode

```bash
export AGENT_CMD='["claude"]'
export AGENT_PROMPT="What is the current working directory?"
export SIDECAR_CLEANUP_TIMEOUT=30
./agentruntime-sidecar
# Runs prompt, exits after 30 seconds of inactivity
```

### Session request with custom environment and credentials

```bash
curl -X POST http://localhost:8090/sessions \
  -H "Content-Type: application/json" \
  -d '{
    "agent": "claude",
    "prompt": "list files",
    "work_dir": "/workspace",
    "env": {
      "DEBUG": "1",
      "WORKSPACE_ROOT": "/workspace",
      "GH_TOKEN": "ghp_xxxxxxxxxxxx"
    }
  }'
```

The `GH_TOKEN` is passed to the Claude Code subprocess inside the container. All other env vars also flow through.

## Security Notes

1. **Clean-room environment (both runtimes):** Both local and Docker runtimes strip AI provider API keys (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `CODEX_API_KEY`) from the agent process environment. This ensures agents authenticate via OAuth credentials (materialized `.credentials.json` / `auth.json`) rather than the operator's personal API keys. API keys override OAuth in Claude Code and Codex, causing agents to hit API rate limits instead of using subscription pricing.

2. **Docker clean-room:** Docker containers additionally receive only the variables in `req.Env`. No other host environment leaks into containers.

3. **Reserved keys:** PATH, LD_PRELOAD, and library path variables cannot be overridden — this prevents library injection attacks.

4. **Network isolation:** On Linux, iptables rules prevent inter-container lateral movement. On macOS, Docker Desktop provides isolation.

5. **Credential hygiene:** Agents authenticate via materialized credential files, not environment variables. Enable `--credential-sync` on the daemon to keep credentials fresh from macOS Keychain. Do not set `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` in `req.Env` — it will override OAuth and hit API rate limits.

6. **Proxy trust:** Agent containers route all egress through the managed Squid proxy sidecar, which runs in the same isolated network.
