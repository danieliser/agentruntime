# agentruntime Implementation Guide

> Developer reference for integrating with and extending agentruntime.
> Generated from source code — March 2026.

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

End-to-end trace of a single session from HTTP request to cleanup.

### 1.1 POST /sessions — Request Validation

`pkg/api/handlers.go:handleCreateSession`

1. **Bind JSON** — `c.ShouldBindJSON(&req)` parses the `SessionRequest`.
2. **Validate agent** — `req.Agent` must be non-empty → 400 `"agent is required"`.
3. **Validate prompt** — Unless `req.Interactive == true`, `req.Prompt` must be non-empty → 400 `"prompt is required"`.
4. **Validate runtime** — If `req.Runtime` is set, it must match the server's runtime (`s.runtime.Name()`) → 400 `"unknown runtime: X"`.

### 1.2 Command Construction

5. **Resolve mounts** — `req.EffectiveMounts()` prepends `WorkDir → /workspace:rw` to explicit `Mounts`.
6. **Resolve workdir** — `effectiveWorkDir(req.WorkDir, mounts)` picks the first rw mount host path, or falls back to the first mount's host path.
7. **Lookup agent** — `s.agents.Get(req.Agent)` resolves from the `DefaultRegistry()` (pre-registered: `"claude"`, `"codex"`) → 400 `"unknown agent: X"`.
8. **Resolve resume session** — `s.lookupResumeSessionID(req.Agent, req.ResumeSession)` calls `agentsessions.ClaudeResumeArgs()` or `agentsessions.CodexResumeArgs()` to find the agent-native session ID from prior session directories.
9. **Build AgentConfig** — Constructs `agent.AgentConfig{WorkDir, Env, Interactive, ResumeSessionID}`.
10. **Build command** — `ag.BuildCmd(prompt, agCfg)` returns the argv slice (e.g., `["claude", "--dangerously-skip-permissions", "-p", "...", "--output-format", "stream-json", ...]`).
11. **Docker cmd override** — For Docker runtime, `spawnCmd` is truncated to `cmd[0]` only — the sidecar handles the full argv via `AGENT_CMD`.

### 1.3 Session Creation & Spawn

12. **Create session** — `session.NewSession(taskID, agent, runtimeName, tags)` allocates a UUID, sets state to `pending`, and creates a lazy `ReplayBuffer` (1 MiB capacity).
13. **Prepare session dir** — `s.prepareSessionDir(sess, &req, workDir)` — for `local` runtime only, calls `agentsessions.InitClaudeSessionDir()` or `agentsessions.InitCodexSessionDir()` to create a per-session agent home under `{dataDir}/{claude,codex}-sessions/{sessionID}/`.
14. **Register session** — `s.sessions.Add(sess)` → 503 if `max_sessions` limit reached.
15. **Spawn process** — `s.runtime.Spawn(ctx, SpawnConfig{...})` returns a `ProcessHandle`. The full `SessionRequest` is passed in `SpawnConfig.Request` for runtimes that need mounts/materialization.
16. **Set running** — `sess.SetRunning(handle)` transitions state to `running`.
17. **Close stdin** — For non-interactive sessions (`!req.Interactive`), `handle.Stdin().Close()` is called immediately — the agent runs with a closed stdin (fire-and-forget mode).
18. **Attach I/O** — `AttachSessionIO(sess, logDir)` starts drain goroutines.

### 1.4 I/O Drain & Logging

`pkg/api/sessionio.go:AttachSessionIO`

19. **Create log writer** — `session.NewLogWriter(logDir, sess.ID)` opens `{logDir}/{sessionID}.ndjson` in append mode.
20. **Create drain target** — `session.DrainWriter(sess.Replay, logw)` returns an `io.MultiWriter` that tees to both the replay buffer and the log file.
21. **Start drain goroutines** — Two goroutines read `handle.Stdout()` and `handle.Stderr()` into the drain target. For sidecar-backed handles (`wsHandle`), stderr returns nil — all output flows through stdout as NDJSON events.
22. **Exit watcher** — A goroutine waits on `handle.Wait()`, then:
    - Waits for drain goroutines to finish.
    - Closes the replay buffer (wakes WaitFor subscribers).
    - Closes the log file.
    - Calls `sess.SetCompleted(result.Code)` → state becomes `completed` (code 0) or `failed` (code != 0).

### 1.5 WebSocket Streaming

`pkg/api/handlers.go:handleSessionWS` + `pkg/bridge/bridge.go`

23. **WS upgrade** — Client connects to `/ws/sessions/:id?since=N`.
24. **Create bridge** — `bridge.New(conn, sess.Handle, sess.Replay)`.
25. **Replay** — If `?since=N` is present, `replay.ReadFrom(N)` returns buffered bytes, sent as a `replay` frame (base64-encoded).
26. **Connected frame** — `{"type":"connected","session_id":"...","mode":"pipe"}`.
27. **Stream pump** — `replayStreamPump` calls `replay.WaitFor(offset)` in a loop, sending `stdout` frames with new data. When the buffer closes (process exited), sends an `exit` frame.
28. **Stdin pump** — `stdinPump` reads client frames and routes them:
    - `stdin` → `handle.Stdin().Write()`
    - `steer` → `SteerableHandle.SendSteer()` (if supported)
    - `interrupt` → `SteerableHandle.SendInterrupt()`
    - `context` → `SteerableHandle.SendContext()`
    - `mention` → `SteerableHandle.SendMention()`
    - `ping` → responds with `pong`

### 1.6 Cleanup

29. **DELETE /sessions/:id** — Calls `sess.Kill()`, closes replay, `sess.SetCompleted(-1)`, removes from registry.
30. **Daemon shutdown** — `sessions.ShutdownAll()` kills all processes, closes replay buffers, then `rt.Cleanup(ctx)` tears down Docker infrastructure.

### State Machine

```text
pending → running → completed (exit 0)
                  → failed    (exit != 0)
                  → orphaned  (recovered after daemon restart)
```

---

## 2. Event Schema Reference

All sidecar events share this envelope:

```json
{
  "type": "agent_message",
  "data": { ... },
  "offset": 12345,
  "timestamp": 1773732712345
}
```

`offset` is the byte position in the replay buffer. `timestamp` is Unix milliseconds. Both are set by the sidecar's `ExternalWSServer.recordAndBroadcast()`.

### 2.1 agent_message

Normalized text output from the agent.

```json
{
  "type": "agent_message",
  "data": {
    "text": "Here's the implementation...",
    "delta": true,
    "model": "claude-opus-4-5",
    "usage": {
      "input_tokens": 1500,
      "output_tokens": 200,
      "cache_read_input_tokens": 500,
      "cache_creation_input_tokens": 0
    },
    "turn_id": "",
    "item_id": ""
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `text` | string | Full text or streaming delta chunk |
| `delta` | bool | `true` = streaming chunk, `false` = final/complete message |
| `model` | string | Model that generated this (Claude only) |
| `usage` | object | Token counts (only on final messages, Claude only) |
| `turn_id` | string | Codex turn identifier |
| `item_id` | string | Codex item identifier |

**Source**: Claude emits `stream_event` (delta) and `assistant` (final). Codex emits `item/agentMessage/delta` (delta) and `item/completed` with type `agent_message` (final).

### 2.2 tool_use

A tool call has started.

```json
{
  "type": "tool_use",
  "data": {
    "id": "toolu_01ABC...",
    "name": "Edit",
    "server": "",
    "input": {
      "file_path": "/workspace/main.go",
      "old_string": "...",
      "new_string": "..."
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Tool call ID (matches `tool_result.id`) |
| `name` | string | Tool name: `Edit`, `Bash`, `Read`, `Write`, `Glob`, `Grep`, etc. |
| `server` | string | MCP server name if this is an MCP tool call (Codex only) |
| `input` | object | Tool arguments |

**Source**: Claude emits `assistant` messages with `content[].type == "tool_use"`. Codex emits `item/started` notifications with item types `command_execution`, `file_change`, or `mcp_tool_call`.

### 2.3 tool_result

A tool call has completed.

```json
{
  "type": "tool_result",
  "data": {
    "id": "toolu_01ABC...",
    "name": "Bash",
    "output": "go build ./...\n",
    "is_error": false,
    "duration_ms": 3500
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Matches the originating `tool_use.id` |
| `name` | string | Tool name |
| `output` | string | Tool output text |
| `is_error` | bool | `true` if the tool errored |
| `duration_ms` | int64 | Execution time in ms (Codex only) |

**Source**: Claude does not currently emit explicit tool_result events at the sidecar level (tool results are internal to the assistant turn). Codex emits `item/completed` for tool items.

### 2.4 result

Turn or session completion summary.

```json
{
  "type": "result",
  "data": {
    "session_id": "abc123-...",
    "turn_id": "",
    "status": "success",
    "cost_usd": 0.0123,
    "duration_ms": 45000,
    "num_turns": 3,
    "usage": {
      "input_tokens": 5000,
      "output_tokens": 1200,
      "cache_read_input_tokens": 2000,
      "cache_creation_input_tokens": 0
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `session_id` | string | Agent's internal session ID (Claude) |
| `turn_id` | string | Turn that completed (Codex) |
| `status` | string | `"success"`, `"error"`, `"interrupted"` |
| `cost_usd` | float64 | Estimated cost (Claude only) |
| `duration_ms` | int64 | Total duration |
| `num_turns` | int | Agentic turns used (Claude only) |
| `usage` | object | Aggregate token counts |

**Source**: Claude emits `{"type":"result",...}` with `subtype`, `cost_usd`, `duration_ms`. Codex emits `turn/completed` notifications.

### 2.5 progress

Progress indicator from the agent.

```json
{
  "type": "progress",
  "data": {
    "type": "progress",
    "message": "Searching files..."
  }
}
```

**Source**: Claude only. The raw Claude `progress` NDJSON line is forwarded as-is.

### 2.6 system

System-level events from the agent or sidecar.

```json
{
  "type": "system",
  "data": {
    "subtype": "stderr",
    "text": "Warning: ..."
  }
}
```

Common `subtype` values:

| Subtype | Source | Description |
|---------|--------|-------------|
| `stderr` | Both | Agent stderr output line |
| `stdout_raw` | Claude | Non-JSON stdout line |
| `thread_started` | Codex | New Codex thread created |
| `hook_*` | Claude | Claude hook execution |
| `agent_error` | Both | Emitted before `exit` when exit code != 0 |

### 2.7 error

An error occurred in the agent or sidecar.

```json
{
  "type": "error",
  "data": {
    "message": "claude stdin unavailable",
    "code": 500
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `message` | string | Error description |
| `code` | int | HTTP-style status code (optional) |

### 2.8 exit

The agent process has terminated.

```json
{
  "type": "exit",
  "exit_code": 0,
  "data": {
    "code": 0,
    "error_detail": ""
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `exit_code` | int | Process exit code (top-level field) |
| `data.code` | int | Same exit code (in data envelope) |
| `data.error_detail` | string | Stderr excerpt or error description on non-zero exit |

**Source**: Both. Emitted by `ExternalWSServer.exitLoop()` after the backend's `Wait()` channel fires.

---

## 3. Steering Reference

Steering commands are sent over the sidecar WebSocket (`/ws` on the sidecar, or via the daemon bridge at `/ws/sessions/:id`).

### 3.1 Sidecar WS Command Format

Commands sent to the sidecar `/ws` endpoint:

```json
{"type": "prompt", "data": {"content": "Fix the bug in auth.go"}}
{"type": "steer",  "data": {"content": "Actually, use JWT instead"}}
{"type": "interrupt"}
{"type": "context", "data": {"text": "...", "filePath": "/workspace/auth.go"}}
{"type": "mention", "data": {"filePath": "/workspace/auth.go", "lineStart": 42, "lineEnd": 50}}
```

### 3.2 Daemon Bridge Frame Format

Commands sent via the daemon `/ws/sessions/:id` endpoint use `ClientFrame`:

```json
{"type": "stdin",     "data": "raw text input\n"}
{"type": "steer",     "data": "Actually, use JWT instead"}
{"type": "interrupt"}
{"type": "context",   "context": {"text": "...", "file_path": "/workspace/auth.go"}}
{"type": "mention",   "mention": {"file_path": "/workspace/auth.go", "line_start": 42, "line_end": 50}}
{"type": "ping"}
{"type": "resize",    "cols": 120, "rows": 40}
```

The bridge translates these to `SteerableHandle` method calls:

| Client Frame | Bridge Method | Sidecar WS Command |
|-------------|---------------|---------------------|
| `stdin` | `handle.Stdin().Write()` | Converted to `prompt` command |
| `steer` | `SteerableHandle.SendSteer()` | `steer` |
| `interrupt` | `SteerableHandle.SendInterrupt()` | `interrupt` |
| `context` | `SteerableHandle.SendContext()` | `context` |
| `mention` | `SteerableHandle.SendMention()` | `mention` |

### 3.3 Claude vs Codex Differences

| Command | Claude Behavior | Codex Behavior |
|---------|----------------|----------------|
| `prompt` | Writes JSONL `{"type":"user","message":{...}}` to Claude's `--input-format stream-json` stdin | Calls `turn/start` JSON-RPC method |
| `steer` | Sends interrupt + new prompt (two operations) | Calls `turn/steer` JSON-RPC method (requires active turn) |
| `interrupt` | Writes `{"type":"control_request","request":{"subtype":"interrupt"}}` to stdin | Calls `turn/interrupt` JSON-RPC method |
| `context` | Sends `selection_changed` notification via MCP server | Logs warning — not supported by Codex app-server |
| `mention` | Sends `at_mentioned` notification via MCP server | Logs warning — not supported by Codex app-server |

### 3.4 Non-Interactive Sessions

For non-interactive sessions (`interactive: false`):
- Stdin is closed immediately after spawn.
- The `wsHandle` converts any `Stdin().Write()` into a `prompt` sidecar command, but this only works if the sidecar is in interactive mode.
- The sidecar receives the prompt via `AGENT_PROMPT` env and runs in fire-and-forget mode.
- Steering a non-interactive session: `steer` and `interrupt` commands will fail on `localHandle` (returns `ErrNotSteerable`). For `wsHandle`-backed sessions, they are forwarded to the sidecar but have no effect in prompt mode since Claude runs with closed stdin.

---

## 4. Runtime Reference

### 4.1 local-pipe (`LocalRuntime`)

**File**: `pkg/runtime/local.go`

**When to use**: Legacy fallback. No sidecar, no structured events. Raw stdout/stderr piped directly. Use when you need the simplest possible integration or are debugging agent output format.

**Registered as**: `--runtime local-pipe` in `cmd/agentd/main.go`.

**SpawnConfig fields used**:

| Field | Used | Notes |
|-------|------|-------|
| `Cmd` | Yes | Full argv passed to `exec.Command` |
| `WorkDir` | Yes | Set as `cmd.Dir` |
| `Env` | Yes | Merged with host env via `buildSpawnEnv()` |
| `PTY` | No | Not supported |
| All others | No | Ignored |

**ProcessHandle type**: `localHandle` — wraps `os/exec.Cmd`.

**Recover()**: Returns `nil, nil` — local processes don't survive daemon restarts.

**Cleanup()**: No-op.

**Known limitations**:
- No structured events — output is raw agent stdout.
- No `SteerableHandle` support — steering commands return `ErrNotSteerable`.
- No session recovery across daemon restarts.
- Inherits host environment (unlike Docker clean-room).

### 4.2 local (`LocalSidecarRuntime`)

**File**: `pkg/runtime/local_sidecar.go`

**When to use**: Default runtime. Runs the sidecar locally for structured events without Docker overhead. Best for development and single-machine deployments.

**Registered as**: `--runtime local` in `cmd/agentd/main.go` (default).

**SpawnConfig fields used**:

| Field | Used | Notes |
|-------|------|-------|
| `Cmd` | Yes | `cmd[0]` becomes `AGENT_CMD` JSON array |
| `Prompt` | Yes | Set as `AGENT_PROMPT` env var |
| `Model` | Yes | Threaded into `AGENT_CONFIG` JSON |
| `WorkDir` | Yes | Set as sidecar process working directory |
| `Env` | Partial | Passed through `AGENT_CONFIG.env` but local sidecar env inherits host via `os.Environ()` — verify against source: `cfg.Env` is not directly merged into the sidecar process env |
| `Request` | Yes | Used by `buildAgentConfigJSON()` to extract model, resume_session, max_turns, allowed_tools, approval_mode |
| `PTY` | No | Not passed to sidecar |

**ProcessHandle type**: `wsHandle` — implements `SteerableHandle`. Connects to sidecar via WebSocket.

**How it works**:
1. Finds a free TCP port via `findFreePort()`.
2. Starts `agentruntime-sidecar` as a subprocess with `AGENT_CMD`, `SIDECAR_PORT`, `AGENT_PROMPT`, and `AGENT_CONFIG` env vars.
3. Health-checks `http://localhost:{port}/health` every 200ms (15s timeout).
4. Dials `ws://localhost:{port}/ws` and creates a `wsHandle`.
5. `wsHandle.killFn` overrides Kill to `sidecar.Process.Kill()`.

**Recover()**: Returns `nil, nil` — local sidecar processes don't survive daemon restarts.

**Cleanup()**: No-op.

**Known limitations**:
- Agent process inherits host `~/.claude/` config (hooks, plugins, MCP servers) — no isolation boundary. The `buildCleanEnv()` in the Claude backend limits env passthrough, but filesystem access is unrestricted.
- Materialization is limited to session dir creation (`prepareSessionDir`) — settings.json, CLAUDE.md, .mcp.json are NOT materialized in local mode (only Docker materializes these). See audit note below.
- `req.Env` from SessionRequest is passed through `AGENT_CONFIG.env` but not directly merged into the sidecar's OS environment.

> **Audit note**: Local mode currently skips full materialization. Settings, CLAUDE.md, MCP config, and memory are not written to the session dir. This means local sessions may behave differently than Docker sessions with the same SessionRequest.

### 4.3 docker (`DockerRuntime`)

**File**: `pkg/runtime/docker.go`

**When to use**: Production deployments. Full isolation, materialized agent config, managed network with egress proxy.

**Registered as**: `--runtime docker` in `cmd/agentd/main.go`.

**SpawnConfig fields used**:

| Field | Used | Notes |
|-------|------|-------|
| `Cmd` | Yes | `cmd[0]` becomes `AGENT_CMD` for the in-container sidecar |
| `Prompt` | Yes | Sent via `dialSidecar()` WS prompt |
| `Model` | Yes | In `AGENT_CONFIG` JSON |
| `Env` | Yes | Written to env-file; clean-room (no host env inheritance) |
| `WorkDir` | Indirect | Resolved from mounts; container always uses `/workspace` |
| `TaskID` | Yes | Docker label `agentruntime.task_id` |
| `SessionID` | Yes | Docker label `agentruntime.session_id` + container name |
| `Request` | Yes | Full materialization: mounts, container config, agent config |
| `SessionDir` | Yes | Updated to materialized session dir path |
| `PTY` | Yes | Adds `-t` flag to `docker run` |

**ProcessHandle type**: `wsHandle` — implements `SteerableHandle`. Kill does `docker stop` + `docker rm`.

**How it works**:
1. `EnsureNetwork()` creates the `agentruntime-agents` Docker bridge network.
2. `EnsureProxy()` starts the `agentruntime-proxy` Squid container for managed egress.
3. `prepareRun()` materializes agent config via `materializer.Materialize()` and builds `docker run` args.
4. Container is started detached (`-d`) with `--rm`, port mapping (`-p 0:9090`), security hardening (`--cap-drop ALL`, `--cap-add DAC_OVERRIDE`, `--security-opt no-new-privileges:true`), labels, env-file, and volume mounts.
5. `dockerContainerPort()` discovers the mapped host port.
6. `waitForDockerSidecarHealth()` polls `http://localhost:{port}/health` (15s timeout, 200ms interval).
7. `dialSidecar()` connects WS and optionally sends the prompt.

**Recover()**: Finds containers with `agentruntime.session_id` label via `docker ps`. For each:
- Tries to `dialSidecar()` via the container's published port.
- Falls back to `newRecoveredDockerHandle()` which follows `docker logs --follow`.
- Returns handles with `RecoveryInfo{SessionID, TaskID}` from container labels.

**Cleanup()**: `NetworkManager.Cleanup()` stops the proxy container and removes the Docker network.

**Security defaults** (applied unless overridden by `ContainerConfig.SecurityOpt`):
```
--cap-drop ALL
--cap-add DAC_OVERRIDE
--security-opt no-new-privileges:true
```

**Known limitations**:
- `Container.Network` from SessionRequest is currently ignored — the managed `agentruntime-agents` network is always used.
- Requires Docker daemon access.
- Container image must have the sidecar binary installed and listening on port 9090.

---

## 5. Adding a New Runtime

### Step 1: Implement the Runtime interface

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
    // 1. Validate cfg.Cmd is non-empty.
    // 2. Start the agent process in your environment.
    // 3. Return a ProcessHandle wrapping the process's stdio.
    return nil, &SpawnError{Reason: "not implemented"}
}

func (r *MyRuntime) Recover(ctx context.Context) ([]ProcessHandle, error) {
    // Find orphaned sessions from a previous daemon run.
    // Return nil, nil if your runtime doesn't support recovery.
    return nil, nil
}

func (r *MyRuntime) Cleanup(ctx context.Context) error {
    // Tear down any runtime-managed infrastructure.
    return nil
}
```

### Step 2: Implement ProcessHandle

```go
type myHandle struct {
    stdin  io.WriteCloser
    stdout io.ReadCloser
    stderr io.ReadCloser
    done   chan ExitResult
}

func (h *myHandle) Stdin() io.WriteCloser   { return h.stdin }
func (h *myHandle) Stdout() io.ReadCloser   { return h.stdout }
func (h *myHandle) Stderr() io.ReadCloser   { return h.stderr }
func (h *myHandle) Wait() <-chan ExitResult  { return h.done }
func (h *myHandle) Kill() error              { /* terminate process */ return nil }
func (h *myHandle) PID() int                 { return 0 }
func (h *myHandle) RecoveryInfo() *RecoveryInfo { return nil }
```

If your runtime uses the sidecar protocol, return a `wsHandle` from `dialSidecar()` instead of implementing a custom handle.

### Step 3: Add compile-time assertion

In `pkg/runtime/runtime.go`:

```go
var _ Runtime = (*MyRuntime)(nil)
```

### Step 4: Register in agentd

In `cmd/agentd/main.go:newRuntime()`:

```go
case "myruntime":
    return runtime.NewMyRuntime(), nil
```

### Step 5: Optionally implement SteerableHandle

If your handle supports the full sidecar command protocol, implement `SteerableHandle`:

```go
type SteerableHandle interface {
    ProcessHandle
    SendPrompt(content string) error
    SendInterrupt() error
    SendSteer(content string) error
    SendContext(text, filePath string) error
    SendMention(filePath string, lineStart, lineEnd int) error
}
```

The bridge checks `handle.(runtime.SteerableHandle)` before routing steer/interrupt/context/mention commands. Non-steerable handles return `ErrNotSteerable`.

---

## 6. Adding a New Agent Backend

Agent backends live in `cmd/sidecar/` and implement the `AgentBackend` interface.

### Step 1: Implement AgentBackend

```go
// cmd/sidecar/myagent.go
package main

import "context"

type MyAgentBackend struct {
    events chan Event
    waitCh chan backendExit
    // ...
}

func (b *MyAgentBackend) Start(ctx context.Context) error {
    // Start the agent process. Parse its output and emit Events.
    return nil
}

func (b *MyAgentBackend) SendPrompt(content string) error   { /* send user input */ return nil }
func (b *MyAgentBackend) SendInterrupt() error              { return nil }
func (b *MyAgentBackend) SendSteer(content string) error    { return nil }
func (b *MyAgentBackend) SendContext(text, filePath string) error   { return nil }
func (b *MyAgentBackend) SendMention(filePath string, lineStart, lineEnd int) error { return nil }
func (b *MyAgentBackend) Events() <-chan Event               { return b.events }
func (b *MyAgentBackend) SessionID() string                  { return "..." }
func (b *MyAgentBackend) Running() bool                      { return true }
func (b *MyAgentBackend) Wait() <-chan backendExit           { return b.waitCh }
```

### Step 2: Register in sidecar main.go

In `cmd/sidecar/main.go:newBackend()`:

```go
case "myagent":
    return NewMyAgentBackend(cmd[0], prompt, cfg), nil
```

### Step 3: Add agent detection

In `cmd/sidecar/main.go:detectAgentType()`:

```go
case strings.Contains(name, "myagent"):
    return "myagent"
```

### Step 4: Add normalization (optional)

In `cmd/sidecar/normalize.go`, add normalization functions if your agent's raw event shapes differ from the standard schema:

```go
func normalizeMyAgentAgentMessage(raw map[string]any) map[string]any {
    return structToMap(NormalizedAgentMessage{
        Text:  stringVal(raw, "text"),
        Delta: raw["streaming"] == true,
    })
}
```

Then wire them into `ExternalWSServer.normalizeEvent()` in `cmd/sidecar/ws.go`:

```go
case "myagent":
    event.Data = normalizeMyAgentAgentMessage(raw)
```

### Step 5: Register the pkg/agent builder

In `pkg/agent/`, create a new agent file and register it in `DefaultRegistry()`:

```go
type MyAgent struct{}

func (a *MyAgent) Name() string { return "myagent" }

func (a *MyAgent) BuildCmd(prompt string, cfg AgentConfig) ([]string, error) {
    return []string{"myagent", "--flag", prompt}, nil
}

func (a *MyAgent) ParseOutput(output []byte) (*AgentResult, bool) {
    return nil, false
}
```

Register in `pkg/agent/agent.go:DefaultRegistry()`:

```go
r.Register(&MyAgent{})
```

---

## 7. SessionRequest Field Reference

| Field | Type | Default | Semantics | Used By |
|-------|------|---------|-----------|---------|
| `task_id` | string | `""` | Caller-assigned task identifier for correlation. Becomes Docker label. | All runtimes |
| `name` | string | `""` | Human-readable session name for observability. Not used by runtime. | Informational |
| `tags` | map[string]string | `nil` | Arbitrary key-value tags. Preserved in session snapshots. | Informational |
| `agent` | string | **required** | Agent identifier: `"claude"` or `"codex"`. Must match a registered agent. | All |
| `runtime` | string | server default | `"local"` or `"docker"`. Must match the daemon's runtime or be empty. | Validation only |
| `model` | string | `""` | Model override (e.g., `"claude-opus-4-5"`, `"o3"`). Passed via `AGENT_CONFIG.model` to sidecar. | Both agents via sidecar |
| `prompt` | string | **required** (unless interactive) | Initial user prompt. Becomes argv for prompt mode or `AGENT_PROMPT` for sidecar. | All |
| `timeout` | string | `"5m"` | Go duration string. Parsed by `EffectiveTimeout()`. **Currently parsed but not enforced at runtime.** | Verify against source |
| `pty` | bool | `false` | Allocate PTY. Adds `-t` to Docker. Not supported by local-pipe. | Docker |
| `interactive` | bool | `false` | Keep stdin open for steering. Suppresses `prompt` in argv. | All |
| `resume_session` | string | `""` | Session ID to resume. Resolved via `agentsessions.ClaudeResumeArgs()` or `CodexResumeArgs()`. | Both agents |
| `work_dir` | string | `""` | Host path. Becomes `Mount{Host: val, Container: "/workspace", Mode: "rw"}`. | All |
| `mounts` | []Mount | `[]` | Additional bind mounts. Merged with WorkDir via `EffectiveMounts()`. | Docker (bind-mounted), Local (workdir only) |
| `claude` | *ClaudeConfig | `nil` | Claude-specific config. Only read when `agent == "claude"`. | Claude |
| `codex` | *CodexConfig | `nil` | Codex-specific config. Only read when `agent == "codex"`. | Codex |
| `mcp_servers` | []MCPServer | `[]` | MCP servers merged into Claude's `.mcp.json`. | Claude (Docker only) |
| `env` | map[string]string | `nil` | Environment variables. Docker: clean-room env-file. Local: passed via `AGENT_CONFIG.env`. | All |
| `container` | *ContainerConfig | `nil` | Docker image, resource limits, security options. Ignored by local runtimes. | Docker only |

### ClaudeConfig Fields

| Field | Type | Default | Semantics |
|-------|------|---------|-----------|
| `settings_json` | map[string]any | `{}` | Written to `~/.claude/settings.json` inside container. `skipDangerousModePermissionPrompt: true` is auto-added. |
| `claude_md` | string | `""` | Written to `~/.claude/CLAUDE.md` inside container. |
| `mcp_json` | map[string]any | `{}` | Base `.mcp.json` merged with `mcp_servers`. `${HOST_GATEWAY}` resolved. |
| `credentials_path` | string | `""` | Host path to `credentials.json`. Copied into session dir. Auto-discovered from credential sync cache if empty. |
| `memory_path` | string | `""` | Host path to Claude memory dir. Mounted read-only at `/home/agent/.claude/projects/{sha256[:16]}`. |
| `max_turns` | int | `0` | `--max-turns` flag for Claude. 0 = unlimited. |
| `allowed_tools` | []string | `[]` | `--allowedTools` flags for Claude. |
| `output_format` | string | ignored | Retained for backward compatibility. Sidecar always uses `stream-json`. |

### CodexConfig Fields

| Field | Type | Default | Semantics |
|-------|------|---------|-----------|
| `config_toml` | map[string]any | `{}` | Written to `~/.codex/config.toml`. Workspace trust and defaults auto-appended. |
| `instructions` | string | `""` | Written to `~/.codex/instructions.md`. |
| `approval_mode` | string | `"full-auto"` | `"full-auto"`, `"auto-edit"`, or `"suggest"`. Maps to Codex approval policy. |

### MCPServer Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Server name (key in `.mcp.json` mcpServers map) |
| `type` | string | `"http"`, `"stdio"`, or `"websocket"` |
| `url` | string | Server URL. Supports `${HOST_GATEWAY}` variable. |
| `cmd` | []string | Command for stdio-type servers |
| `env` | map[string]string | Environment variables for the server |
| `token` | string | Auth token |

### ContainerConfig Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `image` | string | `"agentruntime-agent:latest"` | Docker image |
| `memory` | string | `""` | Docker `--memory` flag (e.g., `"4g"`) |
| `cpus` | float64 | `0` | Docker `--cpus` flag (e.g., `2.0`) |
| `network` | string | `""` | **Currently ignored.** Managed network is always used. |
| `security_opt` | []string | `[]` | Additional `--security-opt` flags |

---

## 8. Credentials & Materialization

### 8.1 Credential Sync

**Package**: `pkg/credentials/`

The daemon optionally syncs credentials in the background when started with `--credential-sync`:

```bash
./agentd --credential-sync
```

This starts `credentials.NewSync(dataDir).Watch(ctx, 30*time.Second)`, which:

1. Every 30 seconds, calls `ClaudeCredentialsFile()`.
2. `ClaudeCredentialsFile()` checks if the cached file (`{dataDir}/credentials/claude-credentials.json`) is fresh (< 30s old).
3. If stale, extracts from the platform credential store and writes to cache.

**Platform extractors**:

| Platform | Extractor | Mechanism |
|----------|-----------|-----------|
| macOS | `keychainExtractor` | `security find-generic-password -s "Claude Code-credentials" -w` |
| Linux | `fileExtractor` | Manual file placement (no system credential store integration) |
| Others | `fileExtractor` | Manual file placement |

**Cache file**: `{dataDir}/credentials/claude-credentials.json` (mode 0600).

**Stale cache fallback**: If extraction fails but a cached file exists, the stale cache is returned. This prevents transient keychain errors from breaking sessions.

**Codex credentials**:
- `CodexCredentialsFile()` checks `~/.codex/auth.json` (OAuth mode).
- `CodexAPIKey()` checks `OPENAI_API_KEY`, then `ANTHROPIC_API_KEY` env vars.

### 8.2 Materialization

**Package**: `pkg/materialize/`

Materialization writes agent-specific config files into a per-session directory and returns Docker bind mounts. Only used by the Docker runtime.

**Entry point**: `materialize.Materialize(req, sessionID, dataDir) → *Result`

**Result struct**:
```go
type Result struct {
    SessionDir string          // path to the created session dir
    Mounts     []apischema.Mount // bind mounts to add to docker run
    CleanupFn  func()          // removes temp files (if dataDir was empty)
}
```

#### Claude Materialization

Creates `{dataDir}/claude-sessions/{sessionID}/` with:

| File | Source | Notes |
|------|--------|-------|
| `settings.json` | `ClaudeConfig.SettingsJSON` | `skipDangerousModePermissionPrompt: true` auto-added |
| `CLAUDE.md` | `ClaudeConfig.ClaudeMD` | Written even if empty |
| `.mcp.json` | `ClaudeConfig.McpJSON` merged with `MCPServers` | `${HOST_GATEWAY}` resolved recursively. URLs validated (http/https/ws/wss only). |
| `credentials.json` + `.credentials.json` | Copied from `CredentialsPath` or auto-discovered | Both names for compatibility |
| `.claude.json` | Auto-generated | Pre-trusts `/workspace`, skips onboarding |

**Mounts created**:
1. `{sessionDir} → /home/agent/.claude (rw)` — the session's Claude config dir
2. `{sessionDir}/.claude.json → /home/agent/.claude.json (rw)` — account state file
3. `{memoryPath} → /home/agent/.claude/projects/{sha256[:16]} (ro)` — if `MemoryPath` set

**Credential auto-discovery** (when `CredentialsPath` is empty):
1. Check `{dataDir}/credentials/claude-credentials.json` (credential sync cache).
2. Check `~/.claude/.credentials.json` or `~/.claude/credentials.json` on the host.

#### Codex Materialization

Creates `{dataDir}/codex-sessions/{sessionID}/` with:

| File | Source | Notes |
|------|--------|-------|
| `config.toml` | `CodexConfig.ConfigTOML` | Workspace trust + defaults auto-appended |
| `instructions.md` | `CodexConfig.Instructions` | Written even if empty |
| `auth.json` | Auto-discovered | From credential cache or `~/.codex/auth.json` |

**Mount created**:
- `{sessionDir} → /home/agent/.codex (rw)`

### 8.3 HOST_GATEWAY Resolution

**File**: `pkg/materialize/gateway.go`

`${HOST_GATEWAY}` in MCP server URLs is resolved at materialization time:

| Platform | Resolved To |
|----------|-------------|
| macOS | `host.docker.internal` |
| Linux | Gateway from `/proc/net/route`, fallback `172.17.0.1` |
| Others | `host.docker.internal` |

Called by `ResolveVars(s)` which does `strings.ReplaceAll(s, "${HOST_GATEWAY}", ResolveHostGateway())`.

---

## 9. Session Resume

### 9.1 How resume_session Works

When `resume_session` is set in the SessionRequest:

1. **Handler**: `handleCreateSession()` calls `s.lookupResumeSessionID(agentName, sessionID)`.
2. **Lookup**: For Claude, calls `agentsessions.ClaudeResumeArgs(dataDir, sessionID)`:
   - Reads `{dataDir}/claude-sessions/{sessionID}/sessions/*.json` (PID-based session index).
   - Falls back to scanning `projects/{hash}/*.jsonl` by mtime.
   - Returns `["--resume", "--session-id", "{claude-native-session-id}"]` or `nil` if first run.
3. **Lookup**: For Codex, calls `agentsessions.CodexResumeArgs(dataDir, sessionID)`:
   - Scans `{dataDir}/codex-sessions/{sessionID}/sessions/` for `.json`/`.jsonl` files.
   - Returns `["--session", "{codex-native-session-id}"]` or `nil`.
4. **Extract**: `resumeSessionIDFromArgs()` extracts the session ID value from the args.
5. **AgentConfig**: The resume session ID is set in `AgentConfig.ResumeSessionID`.
6. **BuildCmd**: The agent's `BuildCmd()` adds `--resume --session-id {id}` (Claude) or `--session {id}` (Codex) to the command line.
7. **Sidecar path**: For sidecar runtimes, `resume_session` is also passed through `AGENT_CONFIG.resume_session` and the sidecar's backend uses it directly.

### 9.2 Replay Buffer Reconnect

The replay buffer enables clients to reconnect without missing output:

1. **First connect**: `GET /ws/sessions/:id` — starts streaming from current position.
2. **Reconnect**: `GET /ws/sessions/:id?since=N` — replays all data from byte offset N.

**Sidecar level**: The sidecar's `ExternalWSServer` also supports `?since=N` on its `/ws` endpoint. When a WS client connects with `since`, the sidecar replays buffered NDJSON events from that offset.

**Daemon level**: The bridge uses `session.ReplayBuffer`:
- `ReadFrom(offset)` returns bytes from `offset` to current position (1 MiB circular buffer).
- `WaitFor(offset)` blocks until new data arrives or the buffer is closed.
- If `offset` is too old (evicted from circular buffer), reading starts from the oldest available byte.

**Log file fallback**: For recovered sessions, `sess.Replay.LoadFromFile(logPath)` pre-populates the replay buffer from the persistent NDJSON log file.

### 9.3 Session Dir Structure

```
{dataDir}/
  claude-sessions/
    {sessionID}/
      settings.json
      CLAUDE.md
      .mcp.json
      credentials.json
      .credentials.json
      .claude.json
      projects/
        -workspace/          # MangleProjectPath("/workspace") = "-workspace"
          {session}.jsonl    # Claude writes session transcripts here
      sessions/
        {pid}.json           # Claude writes PID-based session index
  codex-sessions/
    {sessionID}/
      config.toml
      instructions.md
      auth.json
      sessions/
        {session}.json       # Codex session metadata
  credentials/
    claude-credentials.json  # Credential sync cache
  logs/
    {sessionID}.ndjson       # Persistent NDJSON log of all sidecar events
```

---

## Appendix: API Quick Reference

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Returns `{"status":"ok","runtime":"local"}` |
| POST | `/sessions` | Create a session → `SessionResponse` |
| GET | `/sessions` | List all sessions → `[]SessionSummary` |
| GET | `/sessions/:id` | Get session snapshot |
| GET | `/sessions/:id/info` | Extended session info with paths |
| GET | `/sessions/:id/logs?cursor=N` | Incremental log read. Returns `Agentruntime-Log-Cursor` header. |
| GET | `/sessions/:id/log` | Full NDJSON log file download |
| DELETE | `/sessions/:id` | Kill and remove session |
| GET | `/ws/sessions/:id?since=N` | WebSocket bridge (replay + streaming) |

### AGENT_CONFIG Environment Variable

Serialized as JSON by the daemon, parsed by the sidecar at startup:

```json
{
  "model": "claude-opus-4-5",
  "resume_session": "abc-123",
  "env": {"MY_VAR": "value"},
  "approval_mode": "full-auto",
  "max_turns": 10,
  "allowed_tools": ["Edit", "Bash", "Read"]
}
```

Built by `pkg/runtime/agentconfig.go:buildAgentConfigJSON()` from `SpawnConfig.Request` fields.
