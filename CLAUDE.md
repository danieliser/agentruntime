# CLAUDE.md

@AGENTS.md

## Project Overview

agentruntime is a Go daemon and library for running coding agents behind one API. The current mainline architecture is sidecar-first: `agentd` manages sessions and logs, the selected runtime launches `cmd/sidecar/`, and the sidecar speaks to Claude Code or Codex, normalizes their output, and feeds that event stream back to the daemon.

## Commands

```bash
# Build everything
go build ./...

# Build daemon
go build -o agentd ./cmd/agentd

# Build sidecar
go build -o agentruntime-sidecar ./cmd/sidecar

# Run daemon with local sidecar runtime
PATH="$PWD:$PATH" ./agentd --port 8090 --runtime local

# Run daemon with Docker sidecar runtime
./agentd --port 8090 --runtime docker
```

## Architecture

```text
cmd/agentd/          daemon entrypoint; HTTP API and daemon WS bridge
cmd/sidecar/         v2 sidecar binary; /health and /ws protocol, normalization, Claude/Codex backends
pkg/api/             HTTP routes, session handlers, daemon bridge wiring
pkg/runtime/         local sidecar runtime, Docker runtime, recovery, Docker proxy/network management
pkg/materialize/     per-session Claude/Codex config and MCP materialization
pkg/session/         session manager, replay buffer, NDJSON log persistence
pkg/bridge/          daemon /ws/sessions/:id bridge (stdin, replay, exit)
pkg/agent/           argv builders and legacy parse helpers for supported agents
sdk/                 thin client-facing SDK docs/stubs
```

Runtime flow today:

```text
client -> agentd -> runtime -> agentruntime-sidecar -> Claude/Codex
                             -> normalized events -> replay buffer + NDJSON log -> client
```

Local and Docker both use the same sidecar v2 protocol. Docker adds a managed bridge network, Squid proxy sidecar, and config materialization into the agent container.

## Sidecar Binary

`cmd/sidecar/` builds `agentruntime-sidecar`.

Primary environment variables:

- `AGENT_CMD`: required JSON array describing the agent command, for example `["claude"]` or `["codex"]`
- `AGENT_PROMPT`: optional initial prompt; when set, the sidecar starts in one-shot prompt mode
- `SIDECAR_PORT`: optional listen port, default `9090`

Legacy fallback variables still exist for the v1 PTY path:

- `AGENT_BIN`
- `AGENT_ARGS_JSON`
- `AGENT_ARGS`

If `AGENT_CMD` is present, the sidecar uses the v2 external WS server. If only the legacy variables are present, it falls back to the older PTY bridge.

## Protocols

### Sidecar `/ws`

Command types:

- `prompt`
- `interrupt`
- `steer`
- `context`
- `mention`

Normalized event types:

- `agent_message`
- `tool_use`
- `tool_result`
- `result`
- `progress`
- `system`
- `error`
- `exit`

Every sidecar event is sent as:

```json
{
  "type": "agent_message",
  "data": {},
  "offset": 123,
  "timestamp": 1773732712345
}
```

### Daemon `/ws/sessions/:id`

The daemon bridge is still the public session WebSocket exposed by `agentd`.

Client to daemon:

- `stdin`
- `ping`
- `resize`

Daemon to client:

- `connected`
- `stdout`
- `replay`
- `pong`
- `error`
- `exit`

For sidecar-backed sessions, `stdout` and `replay` carry NDJSON event lines produced by the sidecar.

## Key Interfaces

- `pkg/runtime/runtime.go`: `Runtime`, `SpawnConfig`, `ProcessHandle`
- `pkg/agent/agent.go`: `Agent`, `AgentConfig`, `AgentResult`
- `cmd/sidecar/ws.go`: sidecar command/event envelopes and routing
- `cmd/sidecar/normalize.go`: shared normalized event payloads

## Notes

- `local` is now the local sidecar runtime, not the older pipe-only runtime.
- `local-pipe` still exists as a legacy fallback and does not provide sidecar-normalized events.
- `docker` uses the same sidecar protocol as `local`, but inside `agentruntime-agent:latest`.
- Docker egress is routed through the managed `agentruntime-proxy` container on the `agentruntime-agents` network.
