# Architecture

## Why Go

- **Concurrency model:** Goroutines and channels map directly to the problem of
  multiplexing agent stdio streams over WebSocket connections. Each session needs
  3-4 concurrent I/O loops — Go handles this without callback spaghetti or thread pools.
- **Single binary:** One static binary simplifies deployment, Docker injection, and
  distribution. No interpreter or runtime dependencies.
- **Docker SDK:** The official Docker client is native Go — no FFI or shelling out.
- **Performance:** Low-latency I/O multiplexing with minimal memory overhead per session.

## Topology

```
Client (PAOP, TypeScript service, curl)
  └── HTTP/WS ──→ agentd (Go daemon, port 8090)
                    ├── SessionManager
                    │     └── Session → replay buffer + process handle + state
                    ├── Runtime (local | docker | opensandbox | ssh)
                    │     └── ProcessHandle → agent process (claude, codex, opencode)
                    └── WS Bridge (stdio ↔ WebSocket frames)
                          ├── stdout pump → {type: "stdout", data: ...}
                          ├── stderr pump → {type: "stderr", data: ...}
                          ├── stdin pump ← {type: "stdin", data: ...}
                          └── exit watch → {type: "exit", code: N}
```

## Key design decisions

### Runtime interface is the extension point

Adding a new runtime (e.g., Kubernetes, Firecracker) means implementing three methods:

```go
type Runtime interface {
    Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error)
    Recover(ctx context.Context) ([]ProcessHandle, error)
    Name() string
}
```

The daemon doesn't know or care how the process is created — it only interacts through
`ProcessHandle` (stdin/stdout/stderr + wait + kill).

### Agent definitions are command builders

An `Agent` knows how to construct the CLI command for a specific AI tool. It doesn't
manage processes — that's the runtime's job. This separation means you can run any
agent on any runtime without coupling.

### Replay buffer enables reconnect resilience

All stdout/stderr output is written to a bounded circular buffer (1 MiB default).
When a WebSocket client reconnects with `?since=<byte_offset>`, the bridge replays
missed output before switching to live streaming. No snapshotting, no persistence —
just enough to survive transient disconnects.

### WebSocket bridge is the multiplexer

The bridge connects a `ProcessHandle` to a WebSocket connection:
- Goroutine per stream direction (stdout→WS, stderr→WS, WS→stdin)
- All output simultaneously written to replay buffer
- Ping/pong keepalive (30s interval, 10s timeout)
- Clean shutdown on process exit or client disconnect

### Session lifecycle

```
Pending → Running → Completed (exit 0)
                  → Failed (exit != 0)
                  → Orphaned (daemon restart, recovered by Runtime.Recover)
```

## SDK strategy

Language-specific SDKs (Python, Node, etc.) will be thin HTTP/WebSocket wrappers
generated from an OpenAPI spec. The spec will be added once the API stabilizes.

## Directory structure

```
cmd/agentd/          ← daemon entrypoint
pkg/
  agent/             ← Agent interface + built-in definitions
  runtime/           ← Runtime interface + implementations
  session/           ← Session struct, manager, replay buffer
  bridge/            ← WebSocket bridge (stdio ↔ WS frames)
  api/               ← HTTP + WebSocket server
sdk/
  python/            ← future Python SDK
  node/              ← future Node SDK
```
