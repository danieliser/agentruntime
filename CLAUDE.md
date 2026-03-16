# CLAUDE.md

## Project Overview

agentruntime — Go library and daemon for spawning, streaming, and steering AI agent
processes (Claude Code, Codex, OpenCode) across execution runtimes (local, Docker,
OpenSandbox, SSH). Independent open-source project; PAOP consumes it as a client.

## Commands

```bash
# Build
go build ./...

# Vet
go vet ./...

# Test (with race detection)
go test -v -race -count=1 ./...

# Build daemon binary
go build -o agentd ./cmd/agentd

# Run daemon
./agentd --port 8090 --runtime local
```

## Architecture

```
cmd/agentd/          ← daemon entrypoint (HTTP + WS server)
pkg/
  agent/             ← Agent interface + built-in definitions (claude, codex, opencode)
  runtime/           ← Runtime interface + implementations (local, docker, opensandbox)
  session/           ← Session struct, manager, replay buffer
  bridge/            ← WebSocket bridge (stdio ↔ WS frames)
  api/               ← HTTP + WebSocket server (Gin)
sdk/
  python/            ← future Python SDK (generated from OpenAPI)
  node/              ← future Node SDK (generated from OpenAPI)
```

## Key Interfaces

- **Runtime** (`pkg/runtime/runtime.go`): `Spawn()`, `Recover()`, `Name()` — the extension
  point for adding new execution environments
- **Agent** (`pkg/agent/agent.go`): `BuildCmd()`, `Name()`, `ParseOutput()` — command builders
  for specific AI tools
- **ProcessHandle** (`pkg/runtime/runtime.go`): `Stdin()`, `Stdout()`, `Stderr()`, `Wait()`,
  `Kill()`, `PID()` — runtime-agnostic process access

## Conventions

- Go 1.22, standard library preferred, minimal external deps (Gin, gorilla/websocket, uuid)
- Tests use stdlib `testing` — no testify
- All tests must pass with `-race` flag
- Interfaces over structs for cross-package boundaries
- One runtime implementation per file
- One agent implementation per file
