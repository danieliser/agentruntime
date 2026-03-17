> **Status: Implemented** — This spec was written before implementation. The code in `cmd/sidecar/` is the source of truth. See [IMPLEMENTATION-GUIDE.md](../IMPLEMENTATION-GUIDE.md) for the reference documentation.

# Sidecar v2: Dual-Channel Architecture

## Problem

The v1 sidecar uses PTY to capture agent output, producing TUI escape codes instead of structured data. VS Code solved this years ago using two simultaneous channels. We should do the same.

## Architecture

```
agentd (host)
  └── DockerRuntime.Spawn()
        └── docker run -d -p 0:9090 agentruntime-agent:latest
              └── sidecar v2 (inside container, port 9090)
                    │
                    ├── Channel A: IDE MCP WebSocket (port 9091, localhost only)
                    │     └── Claude connects here via CLAUDE_CODE_SSE_PORT
                    │     └── Context injection, file mentions, tool calls
                    │     └── Lock file at ~/.claude/ide/{port}.lock
                    │
                    ├── Channel B: JSONL stdio bridge
                    │     └── Spawns: claude --output-format stream-json
                    │               --input-format stream-json --verbose
                    │               --dangerously-skip-permissions --ide
                    │     └── stdin: user prompts, control requests (interrupt, steer)
                    │     └── stdout: assistant messages, tool calls, results
                    │
                    └── External WS (port 9090)
                          └── Host agentd connects here
                          └── Merges both channels into unified event stream
                          └── Client sends: prompts, steering, context injection
                          └── Client receives: structured NDJSON events
```

## Agent Integration

### Claude Code

**Spawn command:**
```
claude --output-format stream-json --input-format stream-json \
       --verbose --dangerously-skip-permissions --ide \
       --session-id {session-id}
```

**Dual channel:**
- Channel A (MCP WS): IDE tools + context injection via `selection_changed`, `at_mentioned`
- Channel B (stdio JSONL): All output. Input: `{"type":"user","content":"..."}` for prompts, `{"type":"control_request","request":{"subtype":"interrupt"}}` for steering

**Key stdout event types:**
- `assistant` — response text + tool calls (Anthropic API message format)
- `result` — turn complete (cost, duration, session_id)
- `progress` — tool execution status
- `control_request` — permission prompt (auto-approve in container)
- `system` — system events (hooks, init)

**Key stdin input types:**
- `{"type":"user","content":"fix the bug"}` — send prompt
- `{"type":"control_request","request":{"subtype":"interrupt"}}` — interrupt current turn
- `{"type":"control_response","response":{"request_id":"...","behavior":"allow"}}` — approve tool use

### Codex

**Spawn command:**
```
codex app-server --listen stdio://
```

**Single channel (JSON-RPC over stdio):**
- `initialize` handshake → `thread/start` → `turn/start` for prompts
- `turn/steer` for mid-turn steering
- `turn/interrupt` to cancel
- `thread/resume` for session recovery

**Key server→client events:**
- `item/agentMessage/delta` — streaming text
- `item/completed` — item finished
- `turn/completed` — turn finished (usage stats)
- `thread/started` — thread created

**Key client→server methods:**
- `turn/start` — send prompt (with threadId)
- `turn/steer` — inject mid-turn (with expectedTurnId)
- `turn/interrupt` — cancel active turn

## Sidecar Implementation

### Files to create/modify

| File | Purpose |
|------|---------|
| `cmd/sidecar/main.go` | Rewrite — dual channel orchestrator |
| `cmd/sidecar/claude.go` | Claude-specific: IDE MCP server + stdio JSONL bridge |
| `cmd/sidecar/codex.go` | Codex-specific: app-server JSON-RPC bridge |
| `cmd/sidecar/mcp.go` | IDE MCP WebSocket server (port from PAOP ide_server.py) |
| `cmd/sidecar/ws.go` | External WS endpoint (merges channels for host) |

### main.go — Entry point

```
1. Read AGENT_CMD env (JSON array) — determines agent type
2. Read SIDECAR_PORT env (default 9090)
3. Detect agent: "claude" → Claude mode, "codex" → Codex mode
4. Start external WS server on :9090
5. On WS connect from host:
   a. Claude mode: start IDE MCP server + spawn claude with dual flags
   b. Codex mode: spawn codex app-server, complete handshake
6. Bridge: forward external WS frames to agent stdin, agent stdout to external WS
7. Replay buffer for reconnection
```

### claude.go — Claude dual-channel

```
1. Start IDE MCP server on random port (localhost only)
2. Write lock file to ~/.claude/ide/{port}.lock
3. Spawn claude with:
   --output-format stream-json --input-format stream-json
   --verbose --dangerously-skip-permissions --ide
   --session-id {session-id}
4. Read stdout JSONL → parse → forward to external WS as structured events
5. Read external WS stdin frames → write to claude's stdin as JSONL
6. Handle control requests:
   - Interrupt: send {"type":"control_request","request":{"subtype":"interrupt"}}
   - Approve: auto-send control_response with behavior:"allow"
7. IDE MCP server handles tool calls from Claude (openFile, getDiagnostics, etc.)
8. Context injection: forward selection_changed/at_mentioned from external WS to IDE MCP
```

### codex.go — Codex JSON-RPC

```
1. Spawn codex app-server --listen stdio://
2. Complete initialize handshake
3. On first prompt from external WS:
   - thread/start → get threadId
   - turn/start with prompt
4. On subsequent prompts:
   - turn/start with same threadId (or turn/steer if mid-turn)
5. Forward all server notifications to external WS
6. Handle approval callbacks: auto-approve with accept response
```

### mcp.go — IDE MCP server (ported from PAOP)

Port the PAOP `ide_server.py` to Go:
- Lock file management
- Auth token validation
- MCP initialize handshake
- 12 tool implementations (openFile, getDiagnostics, etc.)
- Ping/pong keepalive (30s)
- Connection replacement on reconnect

### ws.go — External WebSocket

The external WS that agentd connects to:
- `GET /health` — readiness check
- `GET /ws?since=N` — WebSocket endpoint with replay
- Unified event format merging both channels
- ReplayBuffer for reconnection
- Ping/pong keepalive

## External WS Frame Protocol

### Server → Client (sidecar → host)

```json
{"type":"agent_message","data":{"text":"Here's the fix...","tool_calls":[...]}}
{"type":"tool_use","data":{"name":"Edit","input":{...},"id":"toolu_..."}}
{"type":"tool_result","data":{"tool_use_id":"toolu_...","output":"..."}}
{"type":"result","data":{"cost_usd":0.02,"duration_ms":5000,"session_id":"..."}}
{"type":"progress","data":{"status":"running","message":"Reading files..."}}
{"type":"system","data":{"subtype":"init","session_id":"..."}}
{"type":"error","data":{"message":"..."}}
{"type":"exit","exit_code":0}
```

### Client → Server (host → sidecar)

```json
{"type":"prompt","data":{"content":"fix the auth bug"}}
{"type":"interrupt"}
{"type":"steer","data":{"content":"actually focus on the database instead"}}
{"type":"context","data":{"text":"selected code","filePath":"/src/auth.ts"}}
{"type":"mention","data":{"filePath":"/src/db.ts","lineStart":10,"lineEnd":20}}
```

## What stays unchanged

- `pkg/runtime/wshandle.go` — still wraps a WS connection as ProcessHandle
- `pkg/runtime/docker.go` — still spawns container, dials WS
- `pkg/api/` — all endpoints unchanged
- `pkg/session/` — replay buffer, session preservation
- `pkg/materialize/` — config materialization, mounts
- `pkg/credentials/` — credential sync

## Verification

1. `go build ./cmd/sidecar/` compiles
2. Docker build includes new sidecar
3. Claude: POST /sessions → structured NDJSON events stream via WS (not TUI escape codes)
4. Claude: send interrupt + new prompt mid-turn → Claude changes course
5. Claude: context injection via selection_changed → Claude acknowledges
6. Codex: POST /sessions → structured JSON-RPC events stream via WS
7. Codex: turn/steer mid-turn → Codex adjusts
8. Both: session resume works across container restarts
9. Both: replay on reconnect works
