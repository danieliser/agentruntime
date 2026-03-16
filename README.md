# agentruntime

A Go library and daemon for spawning, streaming, and steering AI agent processes
(Claude Code, Codex, OpenCode, and arbitrary CLI agents) across multiple execution
runtimes (local, Docker, SSH, OpenSandbox).

## Why Go

Go's goroutine-based concurrency model is ideal for I/O-heavy workloads like
multiplexing agent stdio over WebSockets. Single binary builds simplify deployment
and container injection. The Docker SDK is native Go.

## Quick start

```bash
go build -o agentd ./cmd/agentd
./agentd --port 8090 --runtime local
```

### API

| Method | Path | Description |
|--------|------|-------------|
| POST | /sessions | Create a session (spawn agent) |
| GET | /sessions/:id | Session status |
| DELETE | /sessions/:id | Kill session |
| GET | /ws/sessions/:id | WebSocket bridge (stdio streaming) |
| GET | /health | Health check |

### WebSocket frames

Connect to `/ws/sessions/:id?since=<byte_offset>` for bidirectional stdio streaming.
Append `?since=0` to replay all buffered output on reconnect.

**Server → Client:** `stdout`, `stderr`, `exit`, `replay`, `connected`, `pong`, `error`
**Client → Server:** `stdin`, `ping`, `resize`

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design.

## License

MIT
