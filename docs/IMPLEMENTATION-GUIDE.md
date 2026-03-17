# agentruntime Implementation Guide

> Developer reference for integrating with and extending agentruntime.
> Generated from source code exploration — March 2026.

---

## Table of Contents

1. [Session Lifecycle](#1-session-lifecycle)
2. [Event Schema Reference](#2-event-schema-reference)
3. [Steering Reference](#3-steering-reference)
4. [Runtime Reference](#4-runtime-reference)
5. [Adding a New Runtime](#5-adding-a-new-runtime)
6. [Adding a New Agent Backend](#6-adding-a-new-agent-backend)
7. [SessionRequest Field Reference](#7-sessionrequest-field-reference)
8. [Credentials & Materialization](#8-credentials--materialization)
9. [Session Resume](#9-session-resume)

---

## 1. Session Lifecycle

End-to-end walkthrough of a single session from creation to cleanup.

### 1.1 POST /sessions — Request Validation

`pkg/api/handlers.go:handleCreateSession`

1. **Bind JSON** — `c.ShouldBindJSON(&req)` parses the `SessionRequest`.
2. **Validate agent** — `req.Agent` must be non-empty. → 400 `"agent is required"`.
3. **Validate prompt** — Unless `req.Interactive == true`, `req.Prompt` must be non-empty. → 400 `"prompt is required"`.
4. **Validate runtime** — If `req.Runtime` is set, it must match the server's runtime (`s.runtime.Name()`). → 400 `"unknown runtime: X"`.

### 1.2 Command Construction

5. **Resolve mounts** — `req.EffectiveMounts()` prepends `WorkDir → /workspace:rw` to explicit `Mounts`.
6. **Resolve workdir** — `effectiveWorkDir(req.WorkDir, mounts)` picks the first rw mount host path, or falls back to the first mount's host path.
7. **Lookup agent** — `s.agents.Get(req.Agent)` resolves from the `DefaultRegistry()` (pre-registered: `"claude"`, `"codex"`). → 400 `"unknown agent: X"`.
8. **Resolve resume** — `s.lookupResumeSessionID(agent, req.ResumeSession)`:
   - Claude: `agentsessions.ClaudeResumeArgs(dataDir, sessionID)` → reads session files → `["--resume", "--session-id", "<native-id>"]`.
   - Codex: `agentsessions.CodexResumeArgs(dataDir, sessionID)` → `["--session", "<native-id>"]`.
9. **Build CLI argv** — `ag.BuildCmd(prompt, agCfg)` produces the agent command line. Example for Claude prompt mode:
   ```
   claude -p "fix the bug" --output-format stream-json --verbose --dangerously-skip-permissions
   ```
10. **Docker command reduction** — If runtime is `"docker"`, `spawnCmd` is truncated to `[]string{cmd[0]}` (e.g., `["claude"]`). The full argv is reconstructed by the sidecar.

### 1.3 Session Creation

11. **New session** — `session.NewSession(taskID, agent, runtime, tags)` creates a `Session` with:
    - `ID`: UUID v4 (generated)
    - `State`: `"pending"`
    - `CreatedAt`: `time.Now()`
    - `Replay`: lazy-allocated 1 MiB `ReplayBuffer`
12. **Prepare session dir** — `s.prepareSessionDir(sess, &req, workDir)`:
    - Only runs for `local` runtime.
    - Claude: `agentsessions.InitClaudeSessionDir(dataDir, sessionID, workDir, credentialsPath)` — creates `{dataDir}/claude-sessions/{sessionID}/` with credentials, `.claude.json`, project subdirs.
    - Codex: `agentsessions.InitCodexSessionDir(dataDir, sessionID)` — creates `{dataDir}/codex-sessions/{sessionID}/sessions/`.
13. **Register session** — `s.sessions.Add(sess)`. May fail with `ErrMaxSessions` → 503.

### 1.4 Process Spawn

14. **Spawn** — `s.runtime.Spawn(ctx, SpawnConfig{...})`:

    **LocalSidecarRuntime** (`pkg/runtime/local_sidecar.go`):
    1. `findFreePort()` — bind `:0`, get port, close.
    2. Marshal `AGENT_CMD=["claude"]`, `SIDECAR_PORT=<port>`, `AGENT_PROMPT=<prompt>`, `AGENT_CONFIG=<json>`.
    3. Start sidecar subprocess: `exec.CommandContext(ctx, "agentruntime-sidecar")`.
    4. Health check loop: poll `GET http://localhost:<port>/health` every 200ms, 15s timeout.
    5. `dialSidecar("local-sidecar-<pid>", "<port>", 0, "")` — WebSocket connect to sidecar.
    6. Return `*wsHandle`.

    **DockerRuntime** (`pkg/runtime/docker.go`):
    1. `EnsureNetwork(ctx)` — create `agentruntime-agents` bridge network.
    2. `EnsureProxy(ctx)` — start `agentruntime-proxy` Squid container.
    3. `prepareRun(cfg)` — resolve image, materialize agent config, build mounts, write env file, assemble `docker run` flags.
    4. `docker run --rm -d -p 0:9090 --init --cap-drop ALL ...` — start detached container.
    5. `dockerContainerPort(ctx, containerID, "9090")` — resolve ephemeral host port.
    6. Health check loop: same protocol as local.
    7. `dialSidecar(containerID, hostPort, 0, prompt)`.
    8. Return `*wsHandle`.

15. **Sidecar boot** (inside `cmd/sidecar/main.go`):
    1. Parse `AGENT_CMD`, `AGENT_CONFIG`, `SIDECAR_PORT`, `AGENT_PROMPT`.
    2. `detectAgentType(cmd)` → `"claude"` | `"codex"` | basename.
    3. `newBackend(agentType, cmd, agentCfg)` → `ClaudeBackend` | `CodexBackend` | `GenericBackend`.
    4. `NewExternalWSServer(agentType, backend)` → v2 WS server.
    5. `http.ListenAndServe(":9090")` — serve `/health` and `/ws`.
    6. **Lazy start**: agent process is NOT spawned until first `/ws` connection (`ensureStarted()`).

### 1.5 Running State

16. **Set running** — `sess.SetRunning(handle)` transitions state `"pending"` → `"running"`.
17. **Close stdin** — For non-interactive sessions (`!req.Interactive`), `handle.Stdin().Close()`. The prompt was already delivered via `AGENT_PROMPT` env var.

### 1.6 IO Attachment

18. **AttachSessionIO** (`pkg/api/sessionio.go`):
    1. Create `LogWriter` → opens `{logDir}/{sessionID}.ndjson` in append mode.
    2. Create `DrainWriter` → `io.MultiWriter(replay, logw)` — tees to both ReplayBuffer and log file.
    3. Start drain goroutines: `drainTo(sessionID, "stdout", handle.Stdout(), drainWriter)` reads 32KB chunks.
    4. Start exit watcher goroutine: waits on `handle.Wait()` → waits for drain to finish (`drainWg.Wait()`) → closes replay buffer → closes log file → `sess.SetCompleted(exitCode)`.

### 1.7 Event Stream

```
Agent process stdout → sidecar normalizeEvent() → recordAndBroadcast()
  → ReplayBuffer (NDJSON line + offset) → broadcast to WS clients
  → wsHandle read goroutine → stdoutW pipe → drainTo() → MultiWriter
    → ReplayBuffer (daemon level) + LogWriter
  → Bridge.replayStreamPump → WS client frames
```

### 1.8 Client Connection

19. **WS upgrade** — Client connects to `GET /ws/sessions/:id?since=<offset>`:
    1. Lookup session. → 404 if missing.
    2. Check `sess.Handle != nil`. → 409 if no active process.
    3. WebSocket upgrade (`4096/4096` buffer, accept all origins).
    4. `bridge.New(conn, handle, replay)` — create bridge.
    5. **Replay**: if `?since=<offset>` is provided, `replay.ReadFrom(offset)` sends catch-up data as a `replay` frame.
    6. Send `connected` frame with `session_id` and `mode: "pipe"`.
    7. Start concurrent loops: `pingLoop` (30s), `replayStreamPump` (blocks on `WaitFor`), `stdinPump` (reads client frames).

### 1.9 Exit & Cleanup

20. **Agent exits** — sidecar detects process exit, broadcasts `exit` event with exit code.
21. **Sidecar cleanup timer** — After agent exit, sidecar starts a timer (default 60s, configurable via `SIDECAR_CLEANUP_TIMEOUT`). New WS connections reset the timer. Timer fires → sidecar HTTP server shuts down → sidecar process exits.
22. **Daemon side** — `wsHandle` read goroutine receives `exit` frame → sends `ExitResult` to `done` channel → `handle.Wait()` unblocks → drain goroutines finish → `sess.SetCompleted(code)` → state becomes `"completed"` (code 0) or `"failed"` (non-zero).
23. **DELETE /sessions/:id** — Manually kill: `sess.Kill()` → `sess.SetCompleted(-1)` → `s.sessions.Remove(sess.ID)`.

### State Machine

```
NewSession() → pending
  → SetRunning(handle) → running
    → SetCompleted(0) → completed
    → SetCompleted(non-zero) → failed
  → Recover() → orphaned
```

---

## 2. Event Schema Reference

All events from the sidecar share this envelope:

```json
{
  "type": "<event_type>",
  "data": { ... },
  "exit_code": null,
  "offset": 12345,
  "timestamp": 1773732712345
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | `string` | Event type identifier |
| `data` | `object` | Type-specific payload (see below) |
| `exit_code` | `int\|null` | Only present on `"exit"` events |
| `offset` | `int64` | Byte offset in the replay buffer |
| `timestamp` | `int64` | Unix milliseconds |

### 2.1 `agent_message`

Text output from the agent. Both Claude and Codex emit this.

```json
{
  "type": "agent_message",
  "data": {
    "text": "I'll fix the authentication module.",
    "delta": false,
    "model": "claude-opus-4-5",
    "usage": {
      "input_tokens": 1500,
      "output_tokens": 200,
      "cache_read_input_tokens": 1200,
      "cache_creation_input_tokens": 0
    },
    "turn_id": "",
    "item_id": ""
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `text` | `string` | Message text content |
| `delta` | `bool` | `true` = streaming chunk (partial). `false` = final/complete message |
| `model` | `string` | Model name (Claude only, empty for Codex) |
| `usage` | `object\|null` | Token usage (only on non-delta messages) |
| `turn_id` | `string` | Codex turn ID (empty for Claude) |
| `item_id` | `string` | Codex item ID (empty for Claude) |

**Source**: Claude `assistant` envelope and `stream_event`. Codex `item/agentMessage/delta` and `item/completed` (agent_message type).

### 2.2 `tool_use`

A tool call has started.

```json
{
  "type": "tool_use",
  "data": {
    "id": "toolu_01abc123",
    "name": "Edit",
    "server": "",
    "input": {
      "file_path": "/workspace/auth.go",
      "old_string": "func login(",
      "new_string": "func Login("
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | `string` | Tool call ID |
| `name` | `string` | Tool name (`"Edit"`, `"Bash"`, etc.) |
| `server` | `string` | MCP server name (if applicable) |
| `input` | `object` | Tool-specific input parameters |

**Source**: Claude `assistant.content[type=tool_use]`. Codex `item/started` for tool items (`commandExecution` → `"Bash"`, `fileChange` → `"Edit"`, `mcp_tool_call` → from `item.tool`).

### 2.3 `tool_result`

A tool call has completed. **Codex only** — Claude does not emit separate tool_result events.

```json
{
  "type": "tool_result",
  "data": {
    "id": "toolu_01abc123",
    "name": "Bash",
    "output": "BUILD SUCCESSFUL",
    "is_error": false,
    "duration_ms": 3400
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | `string` | Corresponding tool_use ID |
| `name` | `string` | Tool name |
| `output` | `string` | Tool execution output |
| `is_error` | `bool` | Whether the tool call failed |
| `duration_ms` | `int` | Execution duration in milliseconds |

**Source**: Codex `item/completed` for tool items. Output extracted from `item.result.content[0].text` or `item.aggregatedOutput`.

### 2.4 `result`

Turn or session completed.

```json
{
  "type": "result",
  "data": {
    "session_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
    "turn_id": "",
    "status": "success",
    "cost_usd": 0.05,
    "duration_ms": 15000,
    "num_turns": 3,
    "usage": null
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `session_id` | `string` | Agent-native session/thread ID |
| `turn_id` | `string` | Codex turn ID (empty for Claude) |
| `status` | `string` | `"success"`, `"error"`, etc. |
| `cost_usd` | `float64` | API cost (Claude only) |
| `duration_ms` | `int` | Session/turn duration (Claude only) |
| `num_turns` | `int` | Number of agentic turns (Claude only) |
| `usage` | `object\|null` | Token usage (Codex only, includes `cached_input_tokens`) |

**Source**: Claude `result` envelope (status from `subtype` field). Codex `turn/completed` notification.

### 2.5 `progress`

Progress indicator from the agent. **Claude only.**

```json
{
  "type": "progress",
  "data": { ... }
}
```

Raw passthrough — data shape is whatever Claude emits. Not normalized.

### 2.6 `system`

System-level events from the agent process.

```json
{
  "type": "system",
  "data": {
    "subtype": "stderr",
    "text": "Warning: deprecated API call"
  }
}
```

| Subtype | Source | Description |
|---------|--------|-------------|
| `stderr` | Both | Stderr line from agent process |
| `stdout_raw` | Claude | Non-JSON line from stdout |
| `thread_started` | Codex | Thread created (`threadId` included) |
| `agent_error` | ws.go | Non-zero exit detected before `exit` event |
| `hook_*` | Claude | Hook-related system events (stripped to subtype only) |

### 2.7 `error`

Error from any source.

```json
{
  "type": "error",
  "data": {
    "message": "JSON parse error on stdout",
    "code": 0
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `message` | `string` | Error description |
| `code` | `int` | Error code (optional, 0 = unspecified) |

### 2.8 `exit`

Agent process has exited. Generated by `ws.go`, not by the agent backend.

```json
{
  "type": "exit",
  "data": {
    "code": 0,
    "error_detail": ""
  },
  "exit_code": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `code` | `int` | Process exit code |
| `error_detail` | `string` | Last 8KB of stderr or sidecar error context |

The top-level `exit_code` field in the event envelope is also set.

### Normalization Matrix

| Event Type | Claude Source | Codex Source | Normalized? |
|-----------|-------------|-------------|:-----------:|
| `agent_message` | `assistant` / `stream_event` | `item/agentMessage/delta`, `item/completed` | Yes |
| `tool_use` | `assistant.content[type=tool_use]` | `item/started` (tool items) | Yes |
| `tool_result` | (not emitted) | `item/completed` (tool items) | Yes (Codex only) |
| `result` | `result` envelope | `turn/completed` | Yes |
| `system` | `system` + stderr | `thread/started` + stderr | No |
| `progress` | `progress` | (not emitted) | No |
| `error` | parse errors, stderr | `error` notification | No |
| `exit` | process exit | process exit | No (ws.go) |

---

## 3. Steering Reference

Steering commands are sent from the client to the daemon WebSocket at `/ws/sessions/:id`, which forwards them through the bridge to the sidecar, which dispatches them to the agent backend.

### 3.1 Command Envelope Format

All commands from client to sidecar share this shape:

```json
{"type": "<command>", "data": { ... }}
```

### 3.2 Commands

#### `prompt`

Send a user message to the agent.

```json
{"type": "prompt", "data": {"content": "Fix the authentication bug"}}
```

| Agent | Behavior |
|-------|----------|
| Claude (interactive) | Writes JSONL to stdin: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}` |
| Claude (prompt) | Not supported — stdin is closed |
| Codex (app-server) | Creates thread (if needed) → `turn/start` JSON-RPC with input |
| Codex (exec) | Not supported — stdin is closed |
| Generic | Writes raw text to stdin |

#### `interrupt`

Cancel the current operation.

```json
{"type": "interrupt"}
```

| Agent | Behavior |
|-------|----------|
| Claude | Writes to stdin: `{"type":"control_request","request":{"subtype":"interrupt"}}` |
| Codex | JSON-RPC: `turn/interrupt {threadId, reason: "user"}`. Requires active turn. |
| Generic | Sends `SIGINT` to process, falls back to `SIGKILL` |

#### `steer`

Redirect the agent mid-conversation. Combines interrupt + new prompt for Claude.

```json
{"type": "steer", "data": {"content": "Focus on the database layer instead"}}
```

| Agent | Behavior |
|-------|----------|
| Claude | `SendInterrupt()` then `SendPrompt(content)` — sequential on stdin |
| Codex | `turn/steer` JSON-RPC: `{threadId, input: [{type:"text", text:"..."}], expectedTurnId}`. Requires active turn (`errCodexNoActiveTurn` if none). |
| Generic | Aliased to `SendPrompt()` — writes raw text to stdin |

#### `context`

Inject editor context into the agent. **Claude interactive only** (via MCP server).

```json
{"type": "context", "data": {"text": "selected code here", "file_path": "/workspace/auth.go"}}
```

| Agent | Behavior |
|-------|----------|
| Claude (interactive) | Routes through MCP server → `selection_changed` notification to Claude |
| Codex | Logs warning, returns nil. **Not supported.** |
| Generic | Returns error: `"not implemented"` |

#### `mention`

Reference a file location. **Claude interactive only** (via MCP server).

```json
{"type": "mention", "data": {"file_path": "/workspace/auth.go", "line_start": 10, "line_end": 25}}
```

| Agent | Behavior |
|-------|----------|
| Claude (interactive) | Routes through MCP server → `at_mentioned` notification to Claude |
| Codex | Logs warning, returns nil. **Not supported.** |
| Generic | Returns error: `"not implemented"` |

### 3.3 Daemon Bridge Frame Format

When connecting via the daemon's `/ws/sessions/:id`, the bridge uses a different frame format than the raw sidecar protocol. The bridge translates between them.

**Client → Daemon:**

```json
{"type": "stdin",     "data": "raw text"}
{"type": "steer",     "data": "new direction"}
{"type": "interrupt"}
{"type": "context",   "context": {"text": "...", "file_path": "..."}}
{"type": "mention",   "mention": {"file_path": "...", "line_start": 10, "line_end": 25}}
{"type": "ping"}
```

**Daemon → Client:**

```json
{"type": "connected", "session_id": "...", "mode": "pipe"}
{"type": "stdout",    "data": "<utf8-or-base64>", "offset": 12345}
{"type": "replay",    "data": "<base64>", "offset": 0}
{"type": "exit",      "exit_code": 0}
{"type": "pong"}
{"type": "error",     "error": "..."}
```

### 3.4 Non-Interactive Sessions

If the session was created without `interactive: true`, steering behavior depends on the handle type:

- **`wsHandle` (SteerableHandle)**: `SendSteer`, `SendInterrupt`, etc. will attempt to send WS frames to the sidecar. Whether the agent responds depends on its mode — prompt-mode agents have stdin closed and may ignore late input.
- **`localHandle`**: Bridge returns `runtime.ErrNotSteerable` for all steerable commands.
- If `handle.Stdin()` was closed (non-interactive session), `stdin` frames will fail silently.

### 3.5 Feature Support Matrix

| Feature | Claude Prompt | Claude Interactive | Codex Exec | Codex App-Server | Generic |
|---------|:---:|:---:|:---:|:---:|:---:|
| prompt | N/A | Yes | N/A | Yes | Yes (raw) |
| interrupt | N/A | Yes | N/A | Yes | Yes (SIGINT) |
| steer | N/A | Yes | N/A | Yes | Yes (raw) |
| context | N/A | Yes (MCP) | N/A | No | No |
| mention | N/A | Yes (MCP) | N/A | No | No |

---

## 4. Runtime Reference

### 4.1 `LocalRuntime` (CLI flag: `local-pipe`)

**Source**: `pkg/runtime/local.go`

**When to use**: Direct pipe-mode execution. No sidecar, no structured events. Raw process stdout/stderr. Useful for agents that don't need event normalization or for debugging.

**ProcessHandle**: `*localHandle` — pipe-based, **not** `SteerableHandle`.

**SpawnConfig fields used**:

| Field | Usage |
|-------|-------|
| `Cmd` | Full command executed via `exec.Command(cmd[0], cmd[1:]...)` |
| `WorkDir` | `cmd.Dir` |
| `Env` | Merged onto `os.Environ()` via `buildSpawnEnv()` |

**Spawn sequence**:
1. Validate `Cmd` non-empty.
2. `exec.CommandContext(ctx, cmd[0], cmd[1:]...)`.
3. `configureLocalProcessGroup(cmd)` — sets `Setpgid: true` on Unix.
4. `buildSpawnEnv(cfg.Env)` — returns `nil` if empty (inherits parent env). Reserved keys blocked: `PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, `DYLD_*`.
5. Open stdin/stdout/stderr pipes.
6. `cmd.Start()`.

**Recovery**: Returns `nil, nil`. Local processes don't survive daemon restarts.

**Cleanup**: No-op.

**Known limitations**:
- No structured events (raw bytes on stdout/stderr).
- No steerable handle — cannot use `steer`, `interrupt`, `context`, `mention` commands.
- No health check.
- No replay normalization.

---

### 4.2 `LocalSidecarRuntime` (CLI flag: `local`, **default**)

**Source**: `pkg/runtime/local_sidecar.go`

**When to use**: Default runtime for development and production use. Runs the sidecar as a local subprocess, providing the same structured NDJSON events and steerable commands as Docker runtime without container overhead.

**ProcessHandle**: `*wsHandle` — implements `SteerableHandle`.

**SpawnConfig fields used**:

| Field | Usage |
|-------|-------|
| `Cmd` | `Cmd[0]` → JSON-marshaled as `AGENT_CMD` env var |
| `Prompt` | Set as `AGENT_PROMPT` env var (if non-empty) |
| `Model` | Threaded into `AGENT_CONFIG` JSON |
| `WorkDir` | `sidecar.Dir` (sidecar working directory) |
| `Request` | Model, ResumeSession, Env, Codex.ApprovalMode → `AGENT_CONFIG` |

**Spawn sequence** (detail in [Section 1.4](#14-process-spawn)):
1. `findFreePort()`.
2. Build env: `AGENT_CMD`, `SIDECAR_PORT`, optionally `AGENT_PROMPT` and `AGENT_CONFIG`.
3. Start `agentruntime-sidecar` subprocess.
4. Health check: `GET /health` every 200ms, 15s timeout.
5. `dialSidecar()` — WS connect, returns `*wsHandle`.

**Recovery**: Returns `nil, nil`. Local sidecars don't survive daemon restarts.

**Cleanup**: No-op.

**Known limitations**:
- Sidecar binary must be in `PATH` or `SidecarBin` must be set explicitly.
- No session recovery across daemon restarts.

---

### 4.3 `DockerRuntime` (CLI flag: `docker`)

**Source**: `pkg/runtime/docker.go`

**When to use**: Production isolation. Runs agent in a Docker container with managed network, Squid proxy for egress, capability dropping, and config materialization.

**ProcessHandle**: `*wsHandle` — implements `SteerableHandle`.

**SpawnConfig fields used**:

| Field | Usage |
|-------|-------|
| `SessionID` | Container name (`agentruntime-<id[:8]>`), label `agentruntime.session_id` |
| `Cmd` | `Cmd[0]` → `AGENT_CMD` JSON in env file |
| `Prompt` | Delivered via WS `SendPrompt` after dial (or last `Cmd` element if `len > 1`) |
| `Env` | Written to docker env file (clean-room, no parent env inheritance) |
| `WorkDir` | Mounted as `/workspace` if no explicit mounts |
| `TaskID` | Label `agentruntime.task_id` |
| `Request` | Mounts, materialization, container image/limits, proxy env |
| `SessionDir` | Output: set to materialized session dir path |
| `PTY` | Adds `-t` flag to `docker run` |

**Docker run flags**:
```
docker run --rm -d
  -p 0:9090
  --init
  --cap-drop ALL
  --cap-add DAC_OVERRIDE
  --security-opt no-new-privileges:true
  --label agentruntime.task_id=<taskID>
  --label agentruntime.session_id=<sessionID>
  --name agentruntime-<sessionID[:8]>
  --workdir /workspace
  --env-file <tmpfile>
  [-t]                              # if PTY
  [-v host:container:mode ...]      # mounts
  [--memory <X>]                    # from Container.Memory
  [--cpus <X>]                      # from Container.CPUs
  [--security-opt <opt> ...]        # from Container.SecurityOpt
  [--network agentruntime-agents]
  <image>
```

**Recovery** (`Recover()`):
1. `docker ps -q --filter label=agentruntime.session_id` — find running containers.
2. For each: extract labels → try WS dial → `*wsHandle` with `RecoveryInfo`.
3. Fallback: `docker logs --follow` → `*recoveredDockerHandle` (read-only, no steering).

**Cleanup** (`Cleanup(ctx)`):
1. `docker stop agentruntime-proxy`.
2. `docker rm -f agentruntime-proxy`.
3. `docker network rm agentruntime-agents`.

**Known limitations**:
- Requires Docker daemon running.
- Image `agentruntime-agent:latest` must be built and available locally.
- Proxy image `agentruntime-proxy:latest` must be built for egress filtering.
- Container port mapping uses ephemeral ports — firewall rules may need adjustment.
- Recovery can only get WS handles for containers whose sidecar is still healthy; otherwise falls back to `docker logs` (no steering).

---

## 5. Adding a New Runtime

### 5.1 Implement the `Runtime` Interface

Create a new file in `pkg/runtime/`:

```go
// pkg/runtime/myruntime.go
package runtime

import "context"

type MyRuntime struct {
    // configuration fields
}

func NewMyRuntime() *MyRuntime {
    return &MyRuntime{}
}

func (r *MyRuntime) Name() string { return "myruntime" }

func (r *MyRuntime) Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error) {
    if len(cfg.Cmd) == 0 {
        return nil, &SpawnError{Reason: "cmd is empty"}
    }

    // 1. Start the agent process in your environment.
    // 2. If using the sidecar, set AGENT_CMD, SIDECAR_PORT, AGENT_PROMPT,
    //    AGENT_CONFIG env vars, then dialSidecar() to get a *wsHandle.
    // 3. If not using the sidecar, return a custom ProcessHandle.

    return nil, &SpawnError{Reason: "not implemented"}
}

func (r *MyRuntime) Recover(ctx context.Context) ([]ProcessHandle, error) {
    // Return handles for any sessions that survived a daemon restart.
    // Return nil, nil if recovery is not supported.
    return nil, nil
}

func (r *MyRuntime) Cleanup(ctx context.Context) error {
    // Tear down any infrastructure your runtime manages.
    return nil
}
```

### 5.2 Implement `ProcessHandle`

If you want structured events and steering, use `dialSidecar()` to get a `*wsHandle` (which implements `SteerableHandle`). If you're doing raw pipe I/O, implement `ProcessHandle` directly:

```go
type myHandle struct {
    stdin  io.WriteCloser
    stdout io.ReadCloser
    stderr io.ReadCloser
    done   chan ExitResult
}

func (h *myHandle) Stdin() io.WriteCloser          { return h.stdin }
func (h *myHandle) Stdout() io.ReadCloser           { return h.stdout }
func (h *myHandle) Stderr() io.ReadCloser           { return h.stderr }
func (h *myHandle) Wait() <-chan ExitResult          { return h.done }
func (h *myHandle) Kill() error                      { /* terminate process */ return nil }
func (h *myHandle) PID() int                         { return 0 }
func (h *myHandle) RecoveryInfo() *RecoveryInfo      { return nil }
```

### 5.3 Register in `cmd/agentd/main.go`

Add your runtime to the `newRuntime()` function:

```go
func newRuntime(name, dataDir string) (runtime.Runtime, error) {
    switch name {
    case "local":
        return runtime.NewLocalSidecarRuntime(), nil
    case "local-pipe":
        return runtime.NewLocalRuntime(), nil
    case "docker":
        return runtime.NewDockerRuntime(runtime.DockerConfig{DataDir: dataDir}), nil
    case "myruntime":
        return runtime.NewMyRuntime(), nil
    default:
        return nil, fmt.Errorf("unknown runtime: %s", name)
    }
}
```

### 5.4 Key Considerations

- **`SpawnError`**: Always wrap spawn failures in `&SpawnError{Reason: "...", Err: err}` for consistent error reporting.
- **Health check**: If using the sidecar, poll `GET /health` every 200ms with a 15s timeout before dialing WS.
- **AGENT_CONFIG**: Use `buildAgentConfigJSON(cfg)` to serialize model, resume_session, env, and approval_mode for the sidecar.
- **Cleanup**: `Cleanup()` is called on graceful daemon shutdown. Make it safe to call even if nothing was started.

---

## 6. Adding a New Agent Backend

Agent backends live in `cmd/sidecar/` and implement the `AgentBackend` interface. Each backend translates a specific agent's native protocol into the unified sidecar event stream.

### 6.1 Implement `AgentBackend`

```go
// cmd/sidecar/myagent.go
package main

import "context"

type myAgentBackend struct {
    binary    string
    prompt    string
    events    chan Event
    sessionID string
    running   bool
    // ...
}

func (b *myAgentBackend) Start(ctx context.Context) error {
    // Spawn the agent process.
    // Parse its output and emit Events on b.events channel.
    return nil
}

func (b *myAgentBackend) SendPrompt(content string) error {
    // Write a user message to the agent's stdin.
    return nil
}

func (b *myAgentBackend) SendInterrupt() error {
    // Interrupt the agent (SIGINT, control message, etc.)
    return nil
}

func (b *myAgentBackend) SendSteer(content string) error {
    // Redirect the agent mid-conversation.
    return nil
}

func (b *myAgentBackend) SendContext(text, filePath string) error {
    // Inject editor context (or return error if unsupported).
    return fmt.Errorf("context injection not supported for myagent")
}

func (b *myAgentBackend) SendMention(filePath string, lineStart, lineEnd int) error {
    // Inject file mention (or return error if unsupported).
    return fmt.Errorf("mentions not supported for myagent")
}

func (b *myAgentBackend) Events() <-chan Event     { return b.events }
func (b *myAgentBackend) SessionID() string         { return b.sessionID }
func (b *myAgentBackend) Running() bool             { return b.running }
func (b *myAgentBackend) Wait() <-chan backendExit  { /* return exit channel */ }
```

### 6.2 Emit Normalized Events

Your backend should emit events on the `Events()` channel using the normalized types from `normalize.go`. The `normalizeEvent()` function in `ws.go` handles the outer envelope — your backend emits raw `Event{Type, Data}` structs and the WS server adds `offset` and `timestamp`.

For full normalization, emit events with normalized data shapes:

```go
// Emit an agent message
b.events <- Event{
    Type: "agent_message",
    Data: NormalizedAgentMessage{
        Text:  "Hello, I'll help with that.",
        Delta: false,
        Model: "myagent-v1",
    },
}

// Emit a tool use
b.events <- Event{
    Type: "tool_use",
    Data: NormalizedToolUse{
        ID:    "tool_123",
        Name:  "Edit",
        Input: map[string]any{"file_path": "/workspace/main.go"},
    },
}

// Emit a result
b.events <- Event{
    Type: "result",
    Data: NormalizedResult{
        SessionID: b.sessionID,
        Status:    "success",
    },
}
```

If your agent emits raw, non-normalized events (like the generic backend), use `"stdout"` and `"stderr"` event types instead. These bypass normalization.

### 6.3 Register in `cmd/sidecar/main.go`

Add your backend to the `newBackend()` function:

```go
func newBackend(agentType string, cmd []string, cfg AgentConfig) (AgentBackend, error) {
    prompt := os.Getenv("AGENT_PROMPT")

    switch agentType {
    case "claude":
        // ... existing
    case "codex":
        // ... existing
    case "myagent":
        return newMyAgentBackend(cmd[0], prompt, cfg), nil
    default:
        return newGenericCommandBackend(agentType, cmd, prompt), nil
    }
}
```

Also update `detectAgentType()` if your binary name needs special detection:

```go
func detectAgentType(cmd []string) string {
    name := strings.ToLower(filepath.Base(cmd[0]))
    switch {
    case strings.Contains(name, "claude"):  return "claude"
    case strings.Contains(name, "codex"):   return "codex"
    case strings.Contains(name, "myagent"): return "myagent"
    default: return name
    }
}
```

### 6.4 Register the Agent in `pkg/agent/`

If your agent should be available via the `agent` field in `SessionRequest`, also create an `Agent` implementation:

```go
// pkg/agent/myagent.go
package agent

type MyAgent struct{}

func (a *MyAgent) Name() string { return "myagent" }

func (a *MyAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
    cmd := []string{"myagent"}
    if prompt != "" {
        cmd = append(cmd, "--prompt", prompt)
    }
    if cfg.Model != "" {
        cmd = append(cmd, "--model", cfg.Model)
    }
    return cmd, nil
}

func (a *MyAgent) ParseOutput(output []byte) (*AgentResult, bool) {
    // Parse agent-specific output format.
    return nil, false
}
```

Register in `DefaultRegistry()` in `pkg/agent/registry.go`:

```go
func DefaultRegistry() *Registry {
    r := NewRegistry()
    r.Register(&ClaudeAgent{})
    r.Register(&CodexAgent{})
    r.Register(&MyAgent{})
    return r
}
```

---

## 7. SessionRequest Field Reference

**Source**: `pkg/api/schema/types.go`

### 7.1 Top-Level Fields

| Field | Type | JSON/YAML | Default | Required | Description |
|-------|------|-----------|---------|:--------:|-------------|
| `task_id` | `string` | `task_id` | `""` | No | Caller-provided correlation ID. Propagated to session, container labels, and responses. |
| `name` | `string` | `name` | `""` | No | Human-readable label for observability. |
| `tags` | `map[string]string` | `tags` | `nil` | No | Arbitrary key-value metadata. Cloned into the session at creation. |
| `agent` | `string` | `agent` | — | **Yes** | Agent name: `"claude"`, `"codex"`. Must match a registered agent. |
| `runtime` | `string` | `runtime` | server default | No | `"local"` or `"docker"`. If set, must match the server's active runtime. |
| `model` | `string` | `model` | `""` | No | Cross-agent model override (e.g., `"claude-opus-4-5"`, `"o3"`). Passed via `AGENT_CONFIG`. |
| `prompt` | `string` | `prompt` | — | **Yes*** | The initial user prompt. *Not required if `interactive: true`. |
| `timeout` | `string` | `timeout` | `"5m"` | No | Go duration string (`"5m"`, `"1h30m"`). Parsed by `EffectiveTimeout()`. Falls back to 5 minutes if empty or unparseable. |
| `pty` | `bool` | `pty` | `false` | No | Allocate PTY. Docker: adds `-t` flag. |
| `interactive` | `bool` | `interactive` | `false` | No | Keep stdin open. Enables steering via WS frames. Prompt not required when `true`. |
| `resume_session` | `string` | `resume_session` | `""` | No | agentruntime session ID to resume. Resolved to agent-native session ID (see [Section 9](#9-session-resume)). |
| `work_dir` | `string` | `work_dir` | `""` | No | Shorthand: becomes `Mount{Host: val, Container: "/workspace", Mode: "rw"}`. |
| `mounts` | `[]Mount` | `mounts` | `nil` | No | Explicit bind-mounts. |
| `claude` | `*ClaudeConfig` | `claude` | `nil` | No | Claude-specific config. Only read when `agent == "claude"`. |
| `codex` | `*CodexConfig` | `codex` | `nil` | No | Codex-specific config. Only read when `agent == "codex"`. |
| `mcp_servers` | `[]MCPServer` | `mcp_servers` | `nil` | No | MCP servers injected into agent config at spawn. |
| `env` | `map[string]string` | `env` | `nil` | No | Clean-room env vars for container. Docker: written to env file, no host env inheritance. Local sidecar: threaded into `AGENT_CONFIG`. |
| `container` | `*ContainerConfig` | `container` | `nil` | No | Image, resource limits, network, security. Docker only. |

### 7.2 `Mount`

| Field | Type | Description |
|-------|------|-------------|
| `host` | `string` | Host path |
| `container` | `string` | Container path |
| `mode` | `string` | `"rw"` or `"ro"` |

### 7.3 `ClaudeConfig`

| Field | Type | JSON | Description |
|-------|------|------|-------------|
| `settings_json` | `map[string]any` | `settings_json` | → `~/.claude/settings.json`. Auto-injects `skipDangerousModePermissionPrompt: true`. |
| `claude_md` | `string` | `claude_md` | → `~/.claude/CLAUDE.md`. Written as plain text. |
| `mcp_json` | `map[string]any` | `mcp_json` | → `~/.claude/.mcp.json`. Merged with top-level `mcp_servers`. URLs sanitized. |
| `credentials_path` | `string` | `credentials_path` | Host path to `credentials.json`. Bind-mounted read-only. If unset, auto-discovered. |
| `memory_path` | `string` | `memory_path` | Host path to Claude project memory. Bind-mounted read-only to `~/.claude/projects/{sha256[:16]}/`. |
| `output_format` | `string` | `output_format` | **Deprecated/ignored** — sidecar always uses `"stream-json"`. |

### 7.4 `CodexConfig`

| Field | Type | JSON | Description |
|-------|------|------|-------------|
| `config_toml` | `map[string]any` | `config_toml` | → `~/.codex/config.toml`. Auto-injects `[projects."/workspace"] trust_level = "trusted"`. |
| `instructions` | `string` | `instructions` | → `~/.codex/instructions.md`. |
| `approval_mode` | `string` | `approval_mode` | `"full-auto"` \| `"auto-edit"` \| `"suggest"`. Passed via `AGENT_CONFIG`. |

### 7.5 `MCPServer`

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Server name (key in `mcpServers` map) |
| `type` | `string` | `"http"` \| `"stdio"` \| `"websocket"` |
| `url` | `string` | Server URL. Supports `${HOST_GATEWAY}` variable substitution. |
| `cmd` | `[]string` | Command for stdio-type servers |
| `env` | `map[string]string` | Environment variables for the server |
| `token` | `string` | Authentication token (control chars stripped) |

### 7.6 `ContainerConfig`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `image` | `string` | `"ubuntu:22.04"` (schema default) / `"agentruntime-agent:latest"` (Docker runtime default) | Container image |
| `memory` | `string` | — | Memory limit (e.g., `"4g"`) |
| `cpus` | `float64` | — | CPU limit |
| `network` | `string` | `"bridge"` | Docker network |
| `security_opt` | `[]string` | — | Additional security options. Base: `--cap-drop ALL --cap-add DAC_OVERRIDE --security-opt no-new-privileges:true`. |

### 7.7 `EffectiveMounts()` Behavior

Returns a new slice. If `WorkDir != ""`, prepends `Mount{Host: WorkDir, Container: "/workspace", Mode: "rw"}`. Then appends all explicit `Mounts`. Does not mutate the original request.

If no mounts are provided and no WorkDir is set, `effectiveWorkDir()` returns `""` and Docker runtime falls back to mounting the working directory as `/workspace`.

### 7.8 `EffectiveTimeout()` Behavior

Parses `Timeout` as a Go duration string (`time.ParseDuration`). Returns the parsed value or `5 * time.Minute` if empty or unparseable. No error surfaced for bad formats — silently falls back.

---

## 8. Credentials & Materialization

### 8.1 Credential Extraction

**Source**: `pkg/credentials/`

Credentials are extracted using platform-specific mechanisms:

| Platform | Build Tag | Mechanism | Keychain Service |
|----------|-----------|-----------|-----------------|
| macOS | `darwin` | `security find-generic-password -s "<service>" -w` | `"Claude Code-credentials"` |
| Linux | `linux` | `secret-tool lookup service "<service>"` (GNOME/KDE); falls back to cached file | Same |
| Other | `!darwin && !linux` | Stub — returns error. Manual placement required. | — |

### 8.2 Credential Caching

`ClaudeCredentialsFile()` behavior:

1. Check cache file: `{dataDir}/credentials/claude-credentials.json`.
2. If fresh (mtime < 30 seconds ago), return cache path immediately.
3. Otherwise, extract from keychain → write to cache file (mode `0600`).
4. If extraction fails but stale cache exists, return stale cache (best-effort).

`CodexCredentialsFile()`: Simply checks `~/.codex/auth.json`. No extraction — expects file to exist.

`CodexAPIKey()`: Checks `OPENAI_API_KEY` env first, falls back to `ANTHROPIC_API_KEY`.

### 8.3 Background Sync

`credentials.NewSync(dataDir).Watch(ctx, interval)` — background goroutine that calls `ClaudeCredentialsFile()` on a 30-second ticker. Keeps the cache file warm. Best-effort: errors are ignored. Cancel via context.

### 8.4 Materialization

**Source**: `pkg/materialize/`

Materialization creates the agent's home directory structure with config files, credentials, and MCP server configs. It runs during `DockerRuntime.Spawn()` when the request has Claude or Codex config.

#### Claude Materialization

Files written to `{dataDir}/claude-sessions/{sessionID}/`:

| File | Source | Notes |
|------|--------|-------|
| `settings.json` | `ClaudeConfig.SettingsJSON` | Auto-injects `skipDangerousModePermissionPrompt: true` |
| `CLAUDE.md` | `ClaudeConfig.ClaudeMD` | Plain text |
| `.mcp.json` | `ClaudeConfig.McpJSON` merged with `MCPServers` | URLs validated (http/https/ws/wss only); `${HOST_GATEWAY}` resolved |
| `.claude.json` | Hardcoded | Pre-trusts `/workspace`, skips onboarding, disables auto-updates |
| `credentials.json` | Auto-discovered or explicit `CredentialsPath` | Copied (not symlinked) |
| `.credentials.json` | Same as above | Legacy compat copy |

Mounts produced:

| Host | Container | Mode |
|------|-----------|------|
| `{claudeDir}` | `/home/agent/.claude` | rw |
| `{claudeDir}/.claude.json` | `/home/agent/.claude.json` | rw |
| `{memoryPath}` (if set) | `/home/agent/.claude/projects/{sha256[:16]}/` | ro |

#### Codex Materialization

Files written to `{dataDir}/codex-sessions/{sessionID}/`:

| File | Source | Notes |
|------|--------|-------|
| `config.toml` | `CodexConfig.ConfigTOML` | Auto-injects `[projects."/workspace"] trust_level = "trusted"` |
| `instructions.md` | `CodexConfig.Instructions` | Plain text |
| `auth.json` | Auto-discovered | Priority: sync cache → `~/.codex/auth.json` |

Mounts produced:

| Host | Container | Mode |
|------|-----------|------|
| `{codexDir}` | `/home/agent/.codex` | rw |

#### Credential Auto-Discovery (during materialization)

When `CredentialsPath` is not explicitly set, the materializer searches:

1. `{dataDir}/credentials/claude-credentials.json` (sync cache)
2. `~/.claude/.credentials.json` (host)
3. `~/.claude/credentials.json` (host)

For Codex: `{dataDir}/credentials/codex-auth.json` (sync cache) → `~/.codex/auth.json` (host).

### 8.5 `HOST_GATEWAY` Resolution

**Source**: `pkg/materialize/gateway.go`

`${HOST_GATEWAY}` in MCP server URLs is resolved at materialization time:

| Platform | Resolution |
|----------|-----------|
| macOS | `host.docker.internal` |
| Linux | Default gateway IP from `/proc/net/route`; fallback `172.17.0.1` |
| Other | `host.docker.internal` |

### 8.6 MCP Config Merging

`buildClaudeMCPJSON()`:
1. Deep-clone base `McpJSON` from `ClaudeConfig`.
2. Extract or create `mcpServers` map.
3. For each `MCPServer` in `req.MCPServers`, convert to map and add to `mcpServers`.
4. Sanitize all URLs (only http/https/ws/wss allowed; empty URLs removed).
5. Strip control characters from tokens.

### 8.7 Security Helpers

| Helper | Purpose |
|--------|---------|
| `sanitizeSessionID()` | Only `[a-zA-Z0-9_-]`, replaces dots/slashes with `-`, trims leading/trailing `-` |
| `stripRelativeTraversal()` | Removes leading `../` after `filepath.Clean` |
| `expandPath()` | Supports `~`, env vars, resolves relative to CWD |

---

## 9. Session Resume

### 9.1 Overview

Session resume allows a new agentruntime session to continue a prior agent-native session (Claude session or Codex thread). The client provides `resume_session` with an agentruntime session ID, and the system resolves it to the agent's native session identifier.

### 9.2 Resume Flow

1. **Client** sends `POST /sessions` with `resume_session: "<agentruntime-session-id>"`.
2. **Handler** calls `s.lookupResumeSessionID(agentName, sessionID)` (`pkg/api/handlers.go`).
3. **Agent-specific lookup**:
   - Claude: `agentsessions.ClaudeResumeArgs(dataDir, sessionID)`:
     1. Resolves `{dataDir}/claude-sessions/{sessionID}/`.
     2. `ReadLastClaudeSessionID(sessionDir)` — reads `sessions/*.json`, sorted by `startedAt` field. Falls back to newest `.jsonl` by mtime.
     3. Returns `["--resume", "--session-id", "<native-claude-session-id>"]`.
   - Codex: `agentsessions.CodexResumeArgs(dataDir, sessionID)`:
     1. Resolves `{dataDir}/codex-sessions/{sessionID}/`.
     2. `ReadLastCodexSessionID(sessionDir)` — walks `sessions/` tree, reads ID from JSON or falls back to filename.
     3. Returns `["--session", "<native-codex-session-id>"]`.
4. **Extract native ID** — `resumeSessionIDFromArgs(args)` scans for `--session` or `--session-id` flag → extracts the following argument.
5. **Build command** — `ResumeSessionID` is set in `AgentConfig`, which `BuildCmd` uses to append resume flags.
6. **Sidecar path** — For sidecar runtimes, `ResumeSession` is also set in `AGENT_CONFIG` JSON. The sidecar's Claude backend maps this to `--resume --session-id <id>`. Codex app-server backend would use it as a thread ID (verify against source — current code generates a fresh UUID unconditionally for Codex `sessionID`).

### 9.3 What State is Preserved

| Agent | Preserved | Not Preserved |
|-------|-----------|---------------|
| Claude | Conversation history, tool results, session context | Replay buffer, agentruntime session metadata |
| Codex | Thread history (app-server mode) | Replay buffer, agentruntime session metadata |

A resumed session is a **new** agentruntime session with a new ID, new replay buffer, and new log file. The agent-native session provides conversation continuity.

### 9.4 Replay Buffer Reconnect

Separate from session resume, the replay buffer supports client reconnection to a **still-running** session:

1. Client connects to `GET /ws/sessions/:id?since=<offset>`.
2. Bridge calls `replay.ReadFrom(offset)` → returns all data from that byte offset.
3. Data is sent as a `replay` frame (base64-encoded).
4. Client receives the `connected` frame, then live `stdout` frames continue from the current offset.

The `offset` field in every `stdout`/`replay` frame tracks the client's position for reconnection.

**Sidecar level**: The sidecar's own `/ws?since=<offset>` endpoint provides the same replay mechanism using a 1 MiB ring buffer. The daemon's replay buffer is an independent layer on top.

### 9.5 Limitations

- Resume is only supported for `"claude"` and `"codex"` agents. Other agents → 400 `"resume_session is not supported for agent: X"`.
- The agentruntime session referenced by `resume_session` must have a session directory with valid agent session files. If the session dir was cleaned up, resume will fail.
- Codex exec mode (prompt mode) does not support resume — it's a one-shot execution.
- Codex app-server resume via thread ID is not fully implemented in the current sidecar — the backend generates a fresh UUID for `sessionID` unconditionally (verify against source for updates).

---

## Appendix A: Backend Interface (Sidecar)

```go
type AgentBackend interface {
    Start(ctx context.Context) error
    SendPrompt(content string) error
    SendInterrupt() error
    SendSteer(content string) error
    SendContext(text, filePath string) error
    SendMention(filePath string, lineStart, lineEnd int) error
    Events() <-chan Event
    SessionID() string
    Running() bool
    Wait() <-chan backendExit
}
```

## Appendix B: Sidecar Server Interface

```go
type sidecarServer interface {
    AgentType() string
    Routes() http.Handler
    Close() error
}

// Optional interfaces:
type cleanupTimeoutConfigurer interface {
    SetCleanupTimeout(time.Duration)
}
type shutdownConfigurer interface {
    SetShutdownFunc(func())
}
type interrupter interface {
    Interrupt() error
}
```

## Appendix C: Sidecar Environment Variables

| Env Var | Required | Default | Description |
|---------|:--------:|---------|-------------|
| `AGENT_CMD` | Yes (v2) | — | JSON array: `["claude"]` or `["codex","--model","o3"]` |
| `AGENT_PROMPT` | No | `""` | Set → prompt mode (fire-and-forget). Empty → interactive mode. |
| `AGENT_CONFIG` | No | `""` | JSON `AgentConfig`: model, resume_session, env, approval_mode, max_turns, allowed_tools |
| `SIDECAR_PORT` | No | `9090` | TCP listen port |
| `SIDECAR_CLEANUP_TIMEOUT` | No | `60s` | Duration before sidecar self-terminates after agent exit |
| `AGENT_BIN` / `AGENT_BINARY` / `AGENT_COMMAND` | Legacy | — | v1 fallback: binary path |
| `AGENT_ARGS_JSON` / `AGENT_ARGS` | Legacy | — | v1 fallback: JSON array or space-separated args |

## Appendix D: AgentConfig Fields (AGENT_CONFIG)

```json
{
  "model": "claude-opus-4-5",
  "resume_session": "native-session-id",
  "env": {"KEY": "VALUE"},
  "approval_mode": "full-auto",
  "max_turns": 10,
  "allowed_tools": ["Read", "Edit", "Bash"]
}
```

| Field | Type | Used By | Description |
|-------|------|---------|-------------|
| `model` | `string` | Both | Model override |
| `resume_session` | `string` | Both | Agent-native session ID to resume |
| `env` | `map[string]string` | Both | Extra env vars merged into agent process |
| `approval_mode` | `string` | Codex | `"full-auto"` \| `"auto-edit"` \| `"suggest"` |
| `max_turns` | `int` | Claude | Maps to `--max-turns` flag |
| `allowed_tools` | `[]string` | Claude | Maps to `--allowedTools` flags |

## Appendix E: HTTP API Quick Reference

| Method | Path | Description | Success |
|--------|------|-------------|:-------:|
| GET | `/health` | Runtime health check | 200 |
| POST | `/sessions` | Create session | 201 |
| GET | `/sessions` | List sessions | 200 |
| GET | `/sessions/:id` | Get session snapshot | 200 |
| GET | `/sessions/:id/info` | Get session info (rich) | 200 |
| GET | `/sessions/:id/logs` | Poll replay buffer (`?cursor=`) | 200 |
| GET | `/sessions/:id/log` | Download full NDJSON log | 200 |
| DELETE | `/sessions/:id` | Kill and remove session | 200 |
| GET | `/ws/sessions/:id` | WebSocket upgrade (`?since=`) | 101 |
