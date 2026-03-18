# Architecture

## Why Go

- **Concurrency model:** goroutines and channels map directly to multiplexing
  agent stdio streams over WebSocket connections. Each session runs 3–4
  concurrent I/O loops without callback spaghetti or thread pools.
- **Single static binary:** one `agentd` binary, one `agentruntime-sidecar`
  binary. No interpreter, no runtime deps. The sidecar binary can be injected
  into Docker containers as-is — CGo-free builds mean zero shared-library
  dependencies.
- **Docker SDK:** the official Docker client is native Go — no FFI or shelling
  out for container management.
- **Low-latency I/O:** minimal memory overhead per session; the replay buffer,
  bridge, and drain goroutines all operate on raw byte slices.

## Two-Tier Topology

```
┌─────────────────────────────────────────────────────────────────────┐
│  Host                                                               │
│                                                                     │
│  Client ──HTTP/WS──→ agentd (:8090)                                │
│                        │                                            │
│                        ├─ SessionManager (state, replay, logs)      │
│                        ├─ Runtime.Spawn(cfg)                        │
│                        │    ├─ local: fork sidecar on host          │
│                        │    └─ docker: docker run -d -p 0:9090 ...  │
│                        │                                            │
│  ┌─────────────────────┼────────────────────────────────────────┐   │
│  │  Container / Host   │  (depends on runtime)                  │   │
│  │                     ▼                                        │   │
│  │  agentruntime-sidecar (:9090)                                │   │
│  │    ├─ /health          HTTP health check                     │   │
│  │    ├─ /ws              event stream + command input           │   │
│  │    ├─ AgentBackend     claude | codex | generic               │   │
│  │    │    └─ stdio ──→ agent process (claude, codex, ...)       │   │
│  │    └─ [MCP server]    localhost-only, Claude --ide mode only  │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  agentd ◄──WS dial localhost:<mapped-port>/ws──► sidecar            │
│         ◄──wsHandle (SteerableHandle)──► normalized NDJSON events   │
└─────────────────────────────────────────────────────────────────────┘
```

In Docker mode, `agentd` also manages a bridge network (`agentruntime-agents`)
and a Squid proxy container (`agentruntime-proxy`) for controlled egress.

## Runtime Interface

The runtime interface is the extension point. Adding a new runtime (Kubernetes,
Firecracker, SSH) means implementing four methods:

```go
type Runtime interface {
    Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error)
    Recover(ctx context.Context) ([]ProcessHandle, error)
    Name() string
    Cleanup(ctx context.Context) error
}
```

`Spawn` creates the agent process and returns a `ProcessHandle` for I/O.
`Recover` finds orphaned sessions from a previous daemon run. `Cleanup` tears
down runtime infrastructure (proxy containers, networks). The daemon never
knows how the process was created — it only interacts through `ProcessHandle`.

### Current Implementations

| CLI flag       | Type                    | Handle type   | Notes |
|----------------|-------------------------|---------------|-------|
| `local` (default) | `LocalSidecarRuntime` | `*wsHandle`   | Forks sidecar on host, dials WS after health check |
| `local-pipe`   | `LocalRuntime`          | `*localHandle`| Legacy direct pipe to agent process, no normalization |
| `docker`       | `DockerRuntime`         | `*wsHandle`   | `docker run` with materialization, network, proxy |

Both `LocalSidecarRuntime` and `DockerRuntime` produce a `*wsHandle`, which
implements `SteerableHandle` — adding `SendPrompt`, `SendInterrupt`,
`SendSteer`, `SendContext`, and `SendMention` on top of the base
`ProcessHandle` (Stdin/Stdout/Stderr/Wait/Kill/PID/RecoveryInfo).

## Sidecar Architecture

The sidecar (`cmd/sidecar/`) is a per-session process that sits between the
runtime and the agent. It exists to solve three problems:

1. **Protocol normalization.** Claude and Codex speak completely different
   protocols (JSONL vs JSON-RPC). The sidecar translates both into a unified
   event schema so the daemon and clients never deal with agent-specific wire
   formats.

2. **Structured control.** The sidecar exposes a WebSocket at `/ws` that
   accepts typed commands (prompt, interrupt, steer, context, mention) and
   emits typed events. This replaces raw stdin/stdout byte wrangling.

3. **IDE simulation.** For Claude's `--ide` mode, the sidecar runs a local MCP
   WebSocket server that presents IDE tools (openFile, getDiagnostics, etc.) so
   Claude behaves as if it's connected to an editor.

The sidecar is started by the runtime, not by the daemon directly. After spawn,
the runtime polls `GET /health` every 200ms (15s timeout) until the sidecar
reports the agent type, then dials `ws://localhost:<port>/ws` to get a
`*wsHandle`. The agent process itself is started lazily — on first WebSocket
connection — not at sidecar boot.

### Environment Variables

| Var | Required | Purpose |
|-----|----------|---------|
| `AGENT_CMD` | Yes (v2) | JSON array of agent command, e.g. `["claude"]` |
| `AGENT_PROMPT` | No | If set, prompt mode (fire-and-forget); if empty, interactive |
| `AGENT_CONFIG` | No | JSON `AgentConfig` (model, resume_session, approval_mode, etc.) |
| `SIDECAR_PORT` | No | Listen port, default `9090` |
| `SIDECAR_CLEANUP_TIMEOUT` | No | Self-terminate delay after agent exit, default `60s` |

## Agent Backends

### Claude (two modes)

**Prompt mode** (`AGENT_PROMPT` set): spawns
`claude -p "<prompt>" --output-format stream-json --verbose --include-partial-messages --dangerously-skip-permissions --session-id <uuid>`.
Stdin closed immediately. One-shot execution. No MCP server.

**Interactive/IDE mode** (`AGENT_PROMPT` empty): spawns
`claude --output-format stream-json --input-format stream-json --verbose --include-partial-messages --dangerously-skip-permissions --ide --session-id <uuid>`.
Stdin stays open for JSONL commands. An MCP WebSocket server runs on a random
localhost port to provide IDE tools. Claude connects to it via
`CLAUDE_CODE_SSE_PORT`. Tool permission requests are auto-approved by the
sidecar.

### Codex (two modes)

**Exec mode** (`AGENT_PROMPT` set): spawns
`codex exec --json --full-auto --skip-git-repo-check "<prompt>"`.
Stdin closed. Flat JSONL output. No handshake, no steering.

**App-server mode** (`AGENT_PROMPT` empty): spawns
`codex app-server --listen stdio://`.
JSON-RPC 2.0 over stdin/stdout. Requires initialization handshake
(`initialize` → `initialized`), then `thread/start` + `turn/start` to begin
work. Supports native steering via `turn/steer` and interrupts via
`turn/interrupt`. Tool approval requests are auto-accepted.

### Generic

Fallback for unknown agent binaries. Raw stdout/stderr lines emitted as
`stdout`/`stderr` events — no normalization. Prompt written to stdin if set.
Interrupt sends SIGINT.

## Unified Event Schema

Every event from the sidecar shares this envelope:

```json
{
  "type": "<event_type>",
  "data": { ... },
  "exit_code": null,
  "offset": 12345,
  "timestamp": 1773732712345
}
```

The `offset` is the byte position in the replay buffer; `timestamp` is Unix
milliseconds.

### Event Types

| Type | Data shape | Source |
|------|-----------|--------|
| `agent_message` | `{text, delta, model, usage, turn_id, item_id}` | Agent text output (streaming or final) |
| `tool_use` | `{id, name, input}` | Tool invocation started |
| `tool_result` | `{id, name, output, is_error, duration_ms}` | Tool completed (Codex only) |
| `result` | `{session_id, turn_id, status, cost_usd, duration_ms, num_turns, usage}` | Turn/session finished |
| `progress` | passthrough `map[string]any` | Claude progress updates |
| `system` | `{subtype, ...}` | Stderr lines, thread_started, hooks |
| `error` | `{message}` | Errors from any source |
| `exit` | `{code, error_detail}` | Agent process exited |

The normalization layer (`cmd/sidecar/normalize.go`) converts agent-specific
output into these shapes. Claude `assistant` envelopes become `agent_message` +
`tool_use` events. Codex JSON-RPC notifications become the same types. Clients
always see the unified schema regardless of backend.

## Session Lifecycle

```
NewSession()         → Pending
  Runtime.Spawn()    → Running   (handle attached)
    exit code 0      → Completed
    exit code != 0   → Failed
  Recover()          → Orphaned  (recovered from previous daemon run)
```

Each session carries a `ReplayBuffer` (1 MiB circular ring buffer) and a
persistent NDJSON log file. All stdout from the process handle is teed to both
via `io.MultiWriter`. An exit watcher goroutine waits on `handle.Wait()`,
drains remaining output, closes the replay buffer, and transitions the session
to its terminal state.

### Recovery

On daemon restart, `Runtime.Recover()` finds orphaned processes:
- **Local runtimes:** return nothing — host processes don't survive restarts.
- **Docker:** queries `docker ps --filter label=agentruntime.session_id`, tries
  to dial each container's sidecar WS. Success → `*wsHandle` with
  `RecoveryInfo`. Failure → fallback `docker logs --follow` handle.

Recovered handles are registered as orphaned sessions. If a log file exists
from the previous run, the replay buffer is populated from it.

## WebSocket Bridge

The bridge (`pkg/bridge/`) connects a session's `ReplayBuffer` to a WebSocket
client. It does not read process pipes directly — it subscribes to the replay
buffer via `WaitFor()`.

```
process → drain goroutine → ReplayBuffer → Bridge → WS client
```

### Frame Types

**Server → client:** `connected`, `stdout`, `replay`, `exit`, `pong`, `error`

**Client → server:** `stdin`, `steer`, `interrupt`, `context`, `mention`,
`ping`, `resize`

The bridge checks whether the `ProcessHandle` implements `SteerableHandle`. If
so, `steer`, `interrupt`, `context`, and `mention` frames are forwarded to the
sidecar. If not, they return `ErrNotSteerable`.

### Reconnect

Clients connect with `?since=<byte_offset>`. The bridge reads the replay buffer
from that offset and sends a `replay` frame before switching to live streaming.
Every `stdout`/`replay` frame includes an `offset` field for position tracking.

### Keepalive

Ping every 30s, pong timeout 10s, write timeout 5s, read timeout 60s.

## Materialization

Before spawning a Docker container, the materializer (`pkg/materialize/`)
writes agent-specific configuration files into a session directory and produces
bind mounts.

### Claude

| File | Source | Notes |
|------|--------|-------|
| `settings.json` | `ClaudeConfig.SettingsJSON` | Auto-injects `skipDangerousModePermissionPrompt: true` |
| `CLAUDE.md` | `ClaudeConfig.ClaudeMD` | Plain text project instructions |
| `.mcp.json` | `ClaudeConfig.McpJSON` merged with `MCPServers` | `${HOST_GATEWAY}` resolved, URLs validated |
| `.claude.json` | Hardcoded | Pre-trusts `/workspace`, skips onboarding |
| `credentials.json` | Explicit path or auto-discovered | From keychain cache, sync cache, or host `~/.claude/` |

### Codex

| File | Source |
|------|--------|
| `config.toml` | `CodexConfig.ConfigTOML` + hardcoded workspace trust |
| `instructions.md` | `CodexConfig.Instructions` |
| `auth.json` | Auto-discovered from sync cache or `~/.codex/auth.json` |

The caller provides configuration content (not file paths) in the
`SessionRequest`. This design means the API is self-contained — clients don't
need to know the agent's filesystem layout, and the materializer handles
platform-specific credential discovery, HOST_GATEWAY resolution, and MCP
config merging.

## Directory Structure

```
cmd/
  agentd/              daemon entrypoint + dispatch CLI client
  sidecar/             sidecar binary: backends, normalization, MCP, WS server
  dashboard/           web dashboard (separate binary)
pkg/
  agent/               Agent interface, ClaudeAgent, CodexAgent, Registry
  api/                 HTTP routes, session handlers, bridge wiring
    schema/            SessionRequest, response types, config sub-types
  bridge/              WebSocket bridge (ReplayBuffer → WS frames)
  client/              Go client for agentd HTTP+WS API
  credentials/         Platform credential extraction (keychain, secret-tool)
  e2e/                 End-to-end test helpers
  materialize/         Pre-spawn config writer (settings, CLAUDE.md, mcp, creds)
  runtime/             Runtime interface + local, local-sidecar, docker impls
  session/             Session struct, Manager, ReplayBuffer, NDJSON log
    agentsessions/     Claude/Codex session dir init and resume helpers
sdk/
  python/              Placeholder (future auto-generated from OpenAPI)
  node/                Placeholder (future auto-generated from OpenAPI)
docs/
  IMPLEMENTATION-GUIDE.md  Developer reference (session lifecycle, event schema)
  architecture-flows.md    Detailed sequence diagrams
  specs/               Protocol and design specifications (historical)
  research/            Agent protocol research notes
  design/              Config shape analysis
```
