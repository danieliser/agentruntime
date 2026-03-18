# agentruntime

agentruntime is a Go daemon and library for running coding agents behind one consistent API. Today that means `agentd` creates and tracks sessions, the runtime launches a v2 `agentruntime-sidecar`, and the sidecar talks to Claude Code or Codex, normalizes their output into a shared event stream, and feeds that stream back through replay buffers and persistent NDJSON logs. The same control plane works locally on the host or inside Docker containers, with Docker adding config materialization and a managed egress proxy.

## Architecture

```text
client
  -> POST /sessions on agentd
  -> GET /ws/sessions/:id or GET /sessions/:id/logs

agentd
  -> session manager + replay buffer + NDJSON log writer
  -> runtime: local or docker

runtime
  -> launches agentruntime-sidecar
  -> local: host process
  -> docker: agentruntime-agent container on managed network + squid proxy

agentruntime-sidecar
  -> starts Claude Code or Codex
  -> speaks /ws using prompt|interrupt|steer|context|mention commands
  -> emits normalized events: agent_message|tool_use|tool_result|result|progress|system|error|exit

agent CLI
  -> raw CLI output
  -> normalized by sidecar
```

## Installation

### Via pip (no Go required)

```bash
pip install agentruntime-agentd
```

This installs the pre-built `agentd` binary for your platform. After installation, `agentd` is available on your PATH:

```bash
agentd --port 8090 --runtime local
```

For programmatic use:

```python
from agentruntime_agentd import get_binary_path

binary = get_binary_path()  # absolute path to the agentd binary
```

### From source

## Quick Start

The default `local` runtime needs both binaries: `agentd` and `agentruntime-sidecar`.

```bash
go build -o agentd ./cmd/agentd
go build -o agentruntime-sidecar ./cmd/sidecar
```

Run the daemon with the sidecar binary on `PATH`:

```bash
PATH="$PWD:$PATH" ./agentd --port 8090 --runtime local
```

Create a prompt-mode session:

```bash
SESSION_JSON=$(curl -sS http://127.0.0.1:8090/sessions \
  -H 'content-type: application/json' \
  -d "{
    \"agent\": \"claude\",
    \"prompt\": \"Reply with exactly hello from agentruntime.\",
    \"work_dir\": \"$PWD\"
  }")

printf '%s\n' "$SESSION_JSON" | jq .
SESSION_ID=$(printf '%s' "$SESSION_JSON" | jq -r '.session_id')
```

Stream output over the daemon WebSocket bridge:

```bash
websocat "ws://127.0.0.1:8090/ws/sessions/$SESSION_ID?since=0"
```

If you prefer polling instead of WebSockets, read the NDJSON stream incrementally:

```bash
curl -sS "http://127.0.0.1:8090/sessions/$SESSION_ID/logs?cursor=0"
```

## Docker

Build the bundled container images:

```bash
./docker/build.sh
```

That script builds:

- `agentruntime-agent:latest`
- `agentruntime-proxy:latest`

You can also build them manually:

```bash
docker build \
  --build-arg HOST_UID="$(id -u)" \
  --build-arg HOST_GID="$(id -g)" \
  -t agentruntime-agent:latest \
  -f docker/Dockerfile.agent \
  .

docker build \
  -t agentruntime-proxy:latest \
  -f docker/Dockerfile.proxy \
  docker
```

Run the daemon in Docker mode:

```bash
go build -o agentd ./cmd/agentd
./agentd --port 8090 --runtime docker
```

What happens in Docker mode:

- `agentd` creates the managed Docker network `agentruntime-agents` if needed.
- `agentd` starts the proxy sidecar container `agentruntime-proxy` if needed.
- agent containers get `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` injected automatically.
- the runtime starts `agentruntime-agent:latest`, which already contains `agentruntime-sidecar`, `claude`, and `codex`.
- Claude and Codex config is materialized into per-session homes under the daemon data directory and mounted into the container.

The default Docker image is `agentruntime-agent:latest`, so a minimal Docker-backed request is still just:

```bash
curl -sS http://127.0.0.1:8090/sessions \
  -H 'content-type: application/json' \
  -d "{
    \"agent\": \"codex\",
    \"prompt\": \"List the top-level files in this repo.\",
    \"work_dir\": \"$PWD\"
  }"
```

## API Reference

### HTTP endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Daemon health and active runtime name |
| `POST` | `/sessions` | Create a session from `SessionRequest` |
| `GET` | `/sessions` | List all known sessions |
| `GET` | `/sessions/:id` | Raw session snapshot from the session manager |
| `GET` | `/sessions/:id/info` | Session summary plus host paths and convenience URLs |
| `GET` | `/sessions/:id/logs?cursor=N` | Incremental replay/log polling; returns `Agentruntime-Log-Cursor` header |
| `GET` | `/sessions/:id/log` | Full persisted NDJSON log download |
| `DELETE` | `/sessions/:id` | Kill the session and mark it completed/failed |
| `GET` | `/ws/sessions/:id?since=N` | Daemon WebSocket bridge for replay plus stdin |

### `POST /sessions`

`POST /sessions` accepts `SessionRequest` JSON and returns:

```json
{
  "session_id": "7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458",
  "task_id": "optional-task-id",
  "agent": "claude",
  "runtime": "local",
  "status": "running",
  "ws_url": "ws://127.0.0.1:8090/ws/sessions/7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458",
  "log_url": "http://127.0.0.1:8090/sessions/7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458/logs"
}
```

Rules enforced by the daemon today:

- `agent` is required.
- `prompt` is required unless `interactive` is `true`.
- `runtime`, if present, must match the daemon runtime selected at startup.
- `work_dir` is shorthand for a writable mount to `/workspace`.

### `GET /sessions/:id`

This returns the raw session snapshot from `pkg/session`, for example:

```json
{
  "id": "7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458",
  "task_id": "optional-task-id",
  "agent_name": "claude",
  "runtime_name": "local",
  "session_dir": "/Users/me/.local/share/agentruntime/claude-sessions/7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458",
  "tags": {
    "repo": "agentruntime"
  },
  "state": "running",
  "created_at": "2026-03-17T07:00:00Z"
}
```

### `GET /sessions/:id/info`

This returns a friendlier API shape with URLs and host paths:

```json
{
  "session_id": "7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458",
  "agent": "claude",
  "runtime": "local",
  "status": "running",
  "created_at": "2026-03-17T07:00:00Z",
  "session_dir": "/Users/me/.local/share/agentruntime/claude-sessions/7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458",
  "log_file": "/Users/me/.local/share/agentruntime/logs/7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458.jsonl",
  "ws_url": "ws://127.0.0.1:8090/ws/sessions/7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458",
  "log_url": "http://127.0.0.1:8090/sessions/7c4f3c3e-8a63-4fe2-baf3-d72b0b7d6458/logs"
}
```

### Daemon WebSocket bridge: `/ws/sessions/:id`

This is the public daemon bridge. It is replay-buffer based and intentionally simpler than the sidecar protocol.

Client to daemon:

- `stdin`: `{ "type": "stdin", "data": "next line of input\n" }`
- `ping`: `{ "type": "ping" }`
- `resize`: `{ "type": "resize", "cols": 120, "rows": 40 }`

Daemon to client:

- `connected`
- `stdout`
- `replay`
- `pong`
- `error`
- `exit`

For sidecar-backed sessions, the `stdout` and `replay` payloads are NDJSON event lines produced by the sidecar.

## WS Protocol

The v2 sidecar has its own WebSocket protocol on `/ws`. Both the local runtime and Docker runtime use it internally, and you can also use it directly if you run `agentruntime-sidecar` yourself.

Command envelope:

```json
{
  "type": "prompt",
  "data": {
    "content": "Fix the failing handler."
  }
}
```

Event envelope:

```json
{
  "type": "agent_message",
  "data": {
    "text": "Looking at the handler now.",
    "delta": true
  },
  "offset": 284,
  "timestamp": 1773732712345
}
```

### Command types

| Type | Payload | Meaning |
| --- | --- | --- |
| `prompt` | `{ "content": "..." }` | Start a turn or send the first user request |
| `interrupt` | none | Interrupt the active turn |
| `steer` | `{ "content": "..." }` | Redirect an in-flight turn without starting over from scratch |
| `context` | `{ "text": "...", "filePath": "/workspace/file.go" }` | Inject selected text plus its file path |
| `mention` | `{ "filePath": "/workspace/file.go", "lineStart": 12, "lineEnd": 30 }` | Inject an IDE-style file mention/range |

### Event types

| Type | Meaning |
| --- | --- |
| `agent_message` | Normalized agent text output; includes streaming deltas and final messages |
| `tool_use` | Normalized tool invocation start |
| `tool_result` | Normalized tool completion |
| `result` | Turn/session result summary |
| `progress` | Intermediate progress from the agent |
| `system` | Lifecycle or stderr-style system notices |
| `error` | Protocol or backend error |
| `exit` | Sidecar process exit notification |

### Normalized payloads

`agent_message` data:

```json
{
  "text": "partial or final text",
  "delta": true,
  "model": "optional-model-name",
  "usage": {
    "input_tokens": 123,
    "output_tokens": 45
  },
  "turn_id": "optional-turn-id",
  "item_id": "optional-item-id"
}
```

`tool_use` data:

```json
{
  "id": "tool-call-id",
  "name": "Bash",
  "server": "optional-mcp-server",
  "input": {
    "command": "git status"
  }
}
```

`tool_result` data:

```json
{
  "id": "tool-call-id",
  "name": "Bash",
  "output": "main.go\nREADME.md\n",
  "is_error": false,
  "duration_ms": 12
}
```

`result` data:

```json
{
  "session_id": "optional-agent-session-id",
  "turn_id": "optional-turn-id",
  "status": "success",
  "cost_usd": 0.0012,
  "duration_ms": 1840,
  "num_turns": 1,
  "usage": {
    "input_tokens": 123,
    "output_tokens": 45
  }
}
```

Notes:

- `offset` is a replay byte offset. Reconnect with `?since=<offset>` to replay from that point.
- not every agent emits every event type on every run.
- Claude emits streaming deltas today.
- Claude emits `tool_use` events; Codex emits both `tool_use` and `tool_result`.

## Context Injection

Context injection is a sidecar v2 feature, not a daemon `/ws/sessions/:id` feature. To use it directly, run the sidecar and talk to its `/ws` endpoint.

Start a sidecar for Claude:

```bash
SIDECAR_PORT=9090 \
AGENT_CMD='["claude"]' \
./agentruntime-sidecar
```

Send a text selection:

```json
{
  "type": "context",
  "data": {
    "text": "func handleCreateSession(...) { ... }",
    "filePath": "/workspace/pkg/api/handlers.go"
  }
}
```

Send a file mention:

```json
{
  "type": "mention",
  "data": {
    "filePath": "/workspace/README.md",
    "lineStart": 1,
    "lineEnd": 40
  }
}
```

Current behavior:

- Claude wires `context` and `mention` into the embedded MCP IDE bridge.
- Codex accepts those commands at the sidecar layer but currently logs a warning and does not inject them into the app-server session.

## Modes

### Prompt vs interactive

- Prompt mode: set `interactive` to `false` or omit it, and include `prompt`. The daemon starts the agent, sends the initial request, and closes stdin for one-shot execution.
- Interactive mode: set `interactive` to `true`. The daemon keeps stdin open, and the agent stays alive for follow-up input. On the daemon bridge, follow-up input uses `stdin`. On the sidecar `/ws`, follow-up control uses `prompt`, `interrupt`, and `steer`.
- `pty` is separate from `interactive`. It asks the runtime for a PTY/TTY allocation; it does not change the sidecar protocol.

### Local vs docker

- Local: `./agentd --runtime local`. The runtime starts `agentruntime-sidecar` on the host and connects to it over localhost.
- Docker: `./agentd --runtime docker`. The runtime starts `agentruntime-agent:latest`, waits for the sidecar health endpoint, then connects to the container over its published port.
- Legacy local pipe mode still exists as `./agentd --runtime local-pipe`, but it bypasses sidecar v2 and does not provide normalized events. New integrations should use `local`.

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) — System architecture and design decisions
- [docs/IMPLEMENTATION-GUIDE.md](docs/IMPLEMENTATION-GUIDE.md) — Developer reference (session lifecycle, event schema, field reference)
- [docs/architecture-flows.md](docs/architecture-flows.md) — Detailed sequence diagrams
- [docs/specs/](docs/specs/) — Design specs (historical)
- [docs/research/](docs/research/) — Protocol research references

## Configuration

`SessionRequest` is the shared request shape used by HTTP, the Go client, and `agentd dispatch --config`.

```json
{
  "task_id": "optional-task-id",
  "name": "optional-label",
  "tags": {
    "repo": "agentruntime",
    "ticket": "DOCS-12"
  },
  "agent": "claude",
  "runtime": "local",
  "model": "optional-model",
  "prompt": "Fix the flaky test.",
  "timeout": "5m",
  "pty": false,
  "interactive": false,
  "resume_session": "optional-agent-native-session-id",
  "work_dir": "/absolute/path",
  "mounts": [
    {
      "host": "/absolute/path",
      "container": "/workspace",
      "mode": "rw"
    }
  ],
  "claude": {
    "settings_json": {},
    "claude_md": "# extra instructions",
    "mcp_json": {},
    "credentials_path": "~/.claude/credentials.json",
    "memory_path": "~/.claude/projects",
    "output_format": "stream-json"
  },
  "codex": {
    "config_toml": {},
    "instructions": "# extra instructions",
    "approval_mode": "suggest"
  },
  "mcp_servers": [
    {
      "name": "docs",
      "type": "http",
      "url": "http://${HOST_GATEWAY}:8080",
      "token": "optional-token"
    }
  ],
  "env": {
    "OPENAI_API_KEY": "set-me"
  },
  "container": {
    "image": "agentruntime-agent:latest",
    "memory": "4g",
    "cpus": 2,
    "security_opt": [
      "label=disable"
    ]
  }
}
```

Fields that matter most in practice:

- `agent`: currently `claude` or `codex` for the v2 sidecar path.
- `prompt` plus `interactive`: choose one-shot or interactive behavior.
- `work_dir` or writable `mounts`: controls `/workspace`.
- `claude` and `codex`: file materialization into `~/.claude` or `~/.codex`.
- `mcp_servers`: merged into Claude MCP config and sanitized during materialization.
- `env`: explicit env vars for the runtime.
- `container.image`, `container.memory`, `container.cpus`, `container.security_opt`: Docker-specific controls that are applied today.

Important implementation notes:

- `work_dir` is shorthand for `{ "host": work_dir, "container": "/workspace", "mode": "rw" }`.
- If both `work_dir` and `mounts` are present, both are used.
- `runtime` is optional and must match the daemon runtime if you send it.
- `${HOST_GATEWAY}` is resolved inside MCP server URLs during materialization.
- The schema currently accepts a few forward-compatible fields that are not wired through end-to-end by `agentd` yet: top-level `name`, `model`, and `timeout`; `claude.output_format`; `codex.approval_mode`; and `container.network`.

### Example: local Claude prompt mode

```json
{
  "agent": "claude",
  "prompt": "Summarize the architecture of this repo in one paragraph.",
  "work_dir": "/Users/me/Toolkit/agentruntime",
  "claude": {
    "claude_md": "Stay focused on this repository."
  }
}
```

### Example: Docker Codex interactive mode

```yaml
agent: codex
interactive: true
work_dir: /Users/me/Toolkit/agentruntime
codex:
  instructions: |
    You are working inside the agentruntime repository.
container:
  image: agentruntime-agent:latest
  memory: 4g
  cpus: 2
env:
  OPENAI_API_KEY: ${OPENAI_API_KEY}
```
