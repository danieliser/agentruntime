# Sidecar Internals — Deep Exploration for Spec Writers

> Generated from a full code read of `cmd/sidecar/*.go` on 2026-03-17.
> Every field name, env var, and event type is quoted directly from source.

---

## 1. Entry Point & Configuration (`main.go`)

### Environment Variables

| Env Var | Required | Default | Description |
|---------|----------|---------|-------------|
| `AGENT_CMD` | Yes (v2) | — | JSON array of the agent command, e.g. `["claude"]` or `["codex","--model","o3"]` |
| `AGENT_PROMPT` | No | `""` | If set, triggers **prompt mode** (fire-and-forget). If empty, **interactive mode**. |
| `AGENT_CONFIG` | No | `""` | JSON-serialized `AgentConfig` struct (model, resume_session, env, approval_mode, max_turns, allowed_tools) |
| `SIDECAR_PORT` | No | `9090` | TCP port the sidecar HTTP server listens on |
| `SIDECAR_CLEANUP_TIMEOUT` | No | `60s` | Duration (seconds or Go duration) before sidecar self-terminates after agent exit |
| `AGENT_BIN` / `AGENT_BINARY` / `AGENT_COMMAND` | Legacy | — | v1 fallback: binary path |
| `AGENT_ARGS_JSON` / `AGENT_ARGS` | Legacy | — | v1 fallback: JSON array or space-separated args |

### Server Selection Flow

```
AGENT_CMD present?
  ├─ Yes → parseAgentCommand(JSON array)
  │        → detectAgentType(cmd) → "claude" | "codex" | basename
  │        → newBackend(agentType, cmd) → AgentBackend
  │        → NewExternalWSServer(agentType, backend) → v2 server
  │
  └─ No  → legacyCommandFromEnv() → AGENT_BIN + AGENT_ARGS
           → newLegacyPTYSidecar(cmd) → v1 server
```

### `sidecarServer` Interface

All servers implement:
```go
type sidecarServer interface {
    AgentType() string
    Routes() http.Handler
    Close() error
}
```

Optional interfaces: `cleanupTimeoutConfigurer`, `shutdownConfigurer`, `interrupter`.

### Shutdown

Signal (SIGINT/SIGTERM) → interrupt agent → `httpServer.Shutdown()` → `server.Close()`.
Cleanup timer fires after agent exit + timeout → triggers shutdown function.

---

## 2. Agent Config Channel (`agentconfig.go`)

Parsed from `AGENT_CONFIG` env var. Fields:

```go
type AgentConfig struct {
    Model         string            `json:"model,omitempty"`
    ResumeSession string            `json:"resume_session,omitempty"`
    Env           map[string]string `json:"env,omitempty"`
    ApprovalMode  string            `json:"approval_mode,omitempty"`
    MaxTurns      int               `json:"max_turns,omitempty"`
    AllowedTools  []string          `json:"allowed_tools,omitempty"`
}
```

- `ResumeSession`: For Claude → `--resume --session-id`; for Codex → thread ID.
- `ApprovalMode`: Claude always uses `--dangerously-skip-permissions`. Codex supports `"full-auto"` | `"auto-edit"` | `"suggest"`.

---

## 3. Claude Backend (`claude.go`)

### Two Modes

#### 3a. Prompt Mode (`AGENT_PROMPT` set)

**Spawn command:**
```
claude -p "<prompt>" --output-format stream-json --verbose --include-partial-messages --dangerously-skip-permissions --session-id <uuid>
```

- Stdin is **closed immediately** after spawn (Claude reads prompt from `-p` flag).
- No MCP server is started.
- No interactive input possible.

#### 3b. Interactive / IDE Mode (`AGENT_PROMPT` empty)

**Spawn command:**
```
claude --output-format stream-json --input-format stream-json --verbose --include-partial-messages --dangerously-skip-permissions --ide --session-id <uuid>
```

- MCP WebSocket server is started first (see §5).
- Environment includes `CLAUDE_CODE_SSE_PORT=<port>`, `ENABLE_IDE_INTEGRATION=true`, `CLAUDE_CODE_EXIT_AFTER_STOP_DELAY=0`.
- Stdin remains open for JSONL input.

### Clean Environment

`buildCleanEnv()` passes through only:
```
PATH, HOME, USER, LANG, TERM, SHELL, TMPDIR, XDG_CONFIG_HOME, XDG_DATA_HOME,
CLAUDE_CODE_OAUTH_TOKEN, OPENAI_API_KEY, ANTHROPIC_API_KEY,
NODE_PATH, NODE_OPTIONS, NVM_DIR,
HTTP_PROXY, HTTPS_PROXY, NO_PROXY, http_proxy, https_proxy, no_proxy
```
Plus explicit extras from MCP server env vars.

### Stdin Protocol (Interactive Mode)

Messages are JSONL (one JSON object per line):

**User prompt:**
```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": [{"type": "text", "text": "<content>"}]
  }
}
```

**Interrupt:**
```json
{
  "type": "control_request",
  "request": {"subtype": "interrupt"}
}
```

**Tool approval auto-response:**
```json
{
  "type": "control_response",
  "response": {
    "request_id": "<id>",
    "behavior": "allow"
  }
}
```

### Stdout Parsing

Claude emits JSONL on stdout. The backend dispatches on `type`:

| Claude stdout `type` | Handler | Emitted sidecar event(s) |
|---------------------|---------|-------------------------|
| `assistant` | `handleAssistant` | `agent_message` (text + usage) + N × `tool_use` |
| `stream_event` | `handleStreamEvent` | `agent_message` with `delta: true` |
| `result` | `handleResult` | `result` (cost_usd, duration_ms, session_id, num_turns, subtype) |
| `progress` | passthrough | `progress` |
| `system` | `handleSystem` | `system` (hook events stripped to subtype only) |
| `control_request` | `handleControlRequest` | No event emitted; auto-responds with `allow` |

**`assistant` envelope structure:**
```json
{
  "type": "assistant",
  "message": {
    "content": [
      {"type": "text", "text": "..."},
      {"type": "tool_use", "id": "...", "name": "...", "input": {...}}
    ],
    "usage": {
      "input_tokens": 0,
      "output_tokens": 0,
      "cache_read_input_tokens": 0,
      "cache_creation_input_tokens": 0
    }
  }
}
```

**`stream_event` envelope structure:**
```json
{
  "type": "stream_event",
  "event": {
    "delta": {"type": "text_delta", "text": "tok"}
  }
}
```

### Steering Flow (Claude)

`SendSteer(content)` = `SendInterrupt()` + `SendPrompt(content)`.
Interrupt sends `control_request.interrupt` via stdin. Then a new user message follows.

### Context & Mentions (Claude)

Both route through the MCP server:
- `SendContext(text, filePath)` → `MCPServer.SendSelection()` → `selection_changed` notification
- `SendMention(filePath, lineStart, lineEnd)` → `MCPServer.SendAtMention()` → `at_mentioned` notification

### Stderr

Lines are emitted as `system` events with `subtype: "stderr"`.
Last 8KB of stderr is buffered for exit error detail.

---

## 4. Codex Backend (`codex.go`)

### Two Modes

#### 4a. Prompt/Exec Mode (`AGENT_PROMPT` set)

**Spawn command:**
```
codex exec --json --full-auto --skip-git-repo-check "<prompt>"
```

- Stdin closed immediately.
- Stdout is **flat JSONL** (not JSON-RPC).
- No handshake, no steering possible.

**Exec JSONL event mapping:**

| Codex exec `type` | Condition | Sidecar event |
|-------------------|-----------|---------------|
| `item.completed` | item.type == `agent_message` | `agent_message` (`final: true`) |
| `item.completed` | item.type != `agent_message` | `tool_result` |
| `item.started` | item.type != `agent_message` | `tool_use` |
| `turn.completed` | — | `result` |
| `thread.started` | — | `system` (`subtype: "thread_started"`) |
| `error` | — | `error` |
| other | — | passthrough with original type |

#### 4b. App-Server Mode (`AGENT_PROMPT` empty)

**Spawn command:**
```
codex app-server --listen stdio://
```

Communication is **JSON-RPC 2.0** over stdin/stdout.

**Initialization handshake:**
1. Client → `initialize` (id: 0) with `clientInfo: {name: "agentruntime", version: "0.3.0"}`, `capabilities: {experimentalApi: true}`
2. Server → response with `userAgent` (required; abort if missing)
3. Client → `initialized` notification (no id)

**JSON-RPC methods used by sidecar:**

| Method | Direction | Purpose |
|--------|-----------|---------|
| `initialize` | client → server | Handshake |
| `initialized` | client → server (notification) | Confirm init |
| `thread/start` | client → server | Create thread, returns `threadId` |
| `turn/start` | client → server | Start a turn with input, approvalPolicy, sandboxPolicy |
| `turn/steer` | client → server | Inject new input during active turn |
| `turn/interrupt` | client → server | Cancel active turn |

**`turn/start` params:**
```json
{
  "threadId": "<id>",
  "input": [{"type": "text", "text": "<content>"}],
  "approvalPolicy": "never",
  "sandboxPolicy": {"type": "dangerFullAccess"}
}
```

**`turn/steer` params:**
```json
{
  "threadId": "<id>",
  "input": [{"type": "text", "text": "<content>"}],
  "expectedTurnId": "<id>"
}
```

**`turn/interrupt` params:**
```json
{"threadId": "<id>", "reason": "user"}
```

**Server requests (auto-handled):**
- `requestApproval` → auto-respond `{"decision": "accept"}`

**Server notifications → sidecar events:**

| Codex notification | Sidecar event | Details |
|-------------------|---------------|---------|
| `thread/started` | `system` | `subtype: "thread_started"`, captures threadId |
| `turn/started` | (internal) | Captures `turnId` for steering |
| `turn/completed` | `result` | Clears `activeTurnID` |
| `item/agentMessage/delta` | `agent_message` | `delta: true`, streaming text |
| `item/started` (tool item) | `tool_use` | Only for `command_execution`, `file_change`, `mcp_tool_call` |
| `item/completed` (agent_message) | `agent_message` | `final: true` |
| `item/completed` (tool item) | `tool_result` | For `command_execution`, `file_change`, `mcp_tool_call` |
| `error` | `error` | — |

### Steering Flow (Codex)

`SendSteer(content)` calls `turn/steer` JSON-RPC with `expectedTurnId`. Requires active turn.

### Context & Mentions (Codex)

**Not supported.** Logs a warning and returns nil.

---

## 5. MCP Server (`mcp.go`)

The MCP server is only started for **Claude interactive mode**. It simulates an IDE for Claude's `--ide` mode.

### Architecture

- Listens on `127.0.0.1:0` (ephemeral port).
- WebSocket endpoint at `/` and `/ws`.
- Auth via `x-claude-code-ide-authorization` header (random UUID token).
- Subprotocol: `mcp`.
- Lock file at `~/.claude/ide/<port>.lock`.

### Lock File Contents

```json
{
  "pid": 12345,
  "workspaceFolders": ["/workspace"],
  "ideName": "agentruntime",
  "transport": "ws",
  "authToken": "<uuid>",
  "port": 54321
}
```

### Environment Variables Set for Claude

```
CLAUDE_CODE_SSE_PORT=<port>
ENABLE_IDE_INTEGRATION=true
CLAUDE_CODE_EXIT_AFTER_STOP_DELAY=0
```

### MCP Protocol

Protocol version: `2025-11-25`.

**Initialization response:**
```json
{
  "protocolVersion": "2025-11-25",
  "capabilities": {
    "tools": {"listChanged": true},
    "resources": {"subscribe": false, "listChanged": false}
  },
  "serverInfo": {"name": "agentruntime", "version": "1.0.0"}
}
```

### Tools Exposed

| Tool Name | Description | Key Args |
|-----------|-------------|----------|
| `openFile` | Open file in editor | `filePath` (required), `preview`, `startText`, `endText`, `selectToEndOfLine`, `makeFrontmost` |
| `openDiff` | Open git diff | `old_file_path`, `new_file_path`, `new_file_contents`, `tab_name` |
| `getCurrentSelection` | Get active editor selection | (none) |
| `getLatestSelection` | Get most recent selection | (none) |
| `getOpenEditors` | List open editor tabs | (none) |
| `getWorkspaceFolders` | Get workspace folders | (none) |
| `getDiagnostics` | Get language diagnostics | `uri` |
| `checkDocumentDirty` | Check unsaved changes | `filePath` (required) |
| `saveDocument` | Save a document | `filePath` (required) |
| `close_tab` | Close tab by name | `tab_name` (required) |
| `closeAllDiffTabs` | Close all diff tabs | (none) |
| `executeCode` | Execute Python in Jupyter | `code` (required) |

**Stubbed behaviors:**
- `openFile` → always returns `{success: true, filePath, preview}`.
- `openDiff` → always returns `"DIFF_REJECTED"`.
- `getCurrentSelection` / `getLatestSelection` → returns last `SendSelection()` value.
- `getOpenEditors` → returns internal `openEditors` list (initially empty).
- `getWorkspaceFolders` → returns configured workspace folders.
- `getDiagnostics` → returns `[]`.
- `checkDocumentDirty` → always `{isDirty: false, isUntitled: false}`.
- `saveDocument` → always `{success: true, saved: true}`.
- `close_tab` → returns `"TAB_CLOSED"`.
- `closeAllDiffTabs` → returns `"CLOSED_0_DIFF_TABS"`.
- `executeCode` → returns `{success: false, message: "Jupyter execution is unavailable in the sidecar"}`.

### Notifications Sent to Claude

| Method | Trigger | Payload |
|--------|---------|---------|
| `selection_changed` | `SendSelection()` / `SendContext()` | `{text, filePath, fileUrl, selection: {start, end, isEmpty}}` |
| `at_mentioned` | `SendAtMention()` / `SendMention()` | `{filePath, lineStart, lineEnd}` |

### Keepalive

WebSocket ping every 30s. Read deadline set to 60s (2× ping interval).

---

## 6. External WebSocket Server (`ws.go`)

This is the **v2 sidecar server** that wraps any `AgentBackend`.

### HTTP Routes

- `GET /health` → JSON health check
- `GET /ws` → WebSocket upgrade (optional `?since=<offset>` for replay)

### Health Response

```json
{
  "status": "ok",          // or "error"
  "agent_running": true,
  "agent_type": "claude",
  "session_id": "<uuid>",
  "error_detail": ""       // only on error
}
```

### WebSocket Protocol

#### Client → Sidecar Commands

| `type` | `data` fields | Backend method |
|--------|---------------|----------------|
| `prompt` | `content: string` | `SendPrompt(content)` |
| `interrupt` | (none) | `SendInterrupt()` |
| `steer` | `content: string` | `SendSteer(content)` |
| `context` | `text: string, filePath: string` | `SendContext(text, filePath)` |
| `mention` | `filePath: string, lineStart: int, lineEnd: int` | `SendMention(filePath, lineStart, lineEnd)` |

Command envelope:
```json
{"type": "prompt", "data": {"content": "Do something"}}
```

#### Sidecar → Client Events

All events share the `Event` envelope:

```json
{
  "type": "<event_type>",
  "data": { ... },
  "exit_code": null,        // only present on "exit"
  "offset": 12345,          // byte offset in replay buffer
  "timestamp": 1773732712345 // Unix milliseconds
}
```

### Event Flow

```
Backend.Events() → eventLoop() → normalizeEvent() → recordAndBroadcast()
                                                      ├─ write to ReplayBuffer (NDJSON)
                                                      └─ writeJSON to all connected clients
```

### Replay / Resume

- Replay buffer: 1MB ring buffer.
- Client connects with `?since=<offset>` → receives all events from that offset before joining live stream.
- Events are stored as NDJSON lines with self-referencing `offset` field.

### Cleanup Timer

After agent exits → start cleanup timer (default 60s, configurable via `SIDECAR_CLEANUP_TIMEOUT`).
New client connections reset the timer. Timer fires → calls shutdown function → sidecar process exits.

### Lazy Start

Backend is started on first WebSocket connection (`ensureStarted()`), not at sidecar startup.

---

## 7. Normalization (`normalize.go`)

The normalization layer converts agent-specific event data into unified shapes. Raw data is **replaced** (not wrapped).

### Normalized Types

#### `NormalizedAgentMessage` (event type: `agent_message`)

```json
{
  "text": "Hello world",
  "delta": false,
  "model": "claude-opus-4-5",
  "usage": {
    "input_tokens": 100,
    "output_tokens": 50,
    "cache_read_input_tokens": 80,
    "cache_creation_input_tokens": 0
  },
  "turn_id": "",
  "item_id": ""
}
```

- `delta: true` = streaming chunk (partial text). `delta: false` = final/complete message.
- `usage` is only present on non-delta messages.
- `turn_id` and `item_id` are Codex-specific (empty for Claude).

#### `NormalizedToolUse` (event type: `tool_use`)

```json
{
  "id": "toolu_01abc",
  "name": "Edit",
  "server": "",
  "input": {"file_path": "/foo.txt", "old_string": "...", "new_string": "..."}
}
```

- Claude: `id`, `name`, `input` from content block.
- Codex: extracted from `item` object. `commandExecution` → `name: "Bash"`, `fileChange` → `name: "Edit"`, `mcp_tool_call` → from `item.tool`.

#### `NormalizedToolResult` (event type: `tool_result`)

```json
{
  "id": "toolu_01abc",
  "name": "Bash",
  "output": "...",
  "is_error": false,
  "duration_ms": 1234
}
```

- Only Codex tool results are normalized (Claude doesn't emit separate tool_result events).
- Output extracted from `item.result.content[0].text` or `item.aggregatedOutput`.

#### `NormalizedResult` (event type: `result`)

```json
{
  "session_id": "<uuid>",
  "turn_id": "",
  "status": "success",
  "cost_usd": 0.05,
  "duration_ms": 15000,
  "num_turns": 3,
  "usage": null
}
```

- Claude: `status` from `subtype` field, `cost_usd`, `duration_ms`, `num_turns`, `session_id`.
- Codex: `status` from `turn.status`, `usage` from `usage` object with `cached_input_tokens`.

### Normalization Matrix

| Event Type | Claude Source | Codex Source | Normalized? |
|-----------|-------------|-------------|-------------|
| `agent_message` | `assistant` envelope / `stream_event` | `item/agentMessage/delta`, `item/completed` | Yes |
| `tool_use` | `assistant.content[type=tool_use]` | `item/started` (tool items) | Yes |
| `tool_result` | (not emitted) | `item/completed` (tool items) | Yes (Codex only) |
| `result` | `result` envelope | `turn/completed` | Yes |
| `system` | `system` + stderr | `thread/started` + stderr | No |
| `progress` | `progress` | (not emitted) | No |
| `error` | parse errors, stderr | `error` notification | No |
| `exit` | process exit | process exit | No (generated by ws.go) |

---

## 8. Unified Event Schema

Every event broadcast to clients has this envelope:

```json
{
  "type": "<string>",
  "data": { ... },
  "exit_code": <int|null>,
  "offset": <int64>,
  "timestamp": <int64>
}
```

### Complete Event Type Catalog

| Type | Data Shape | Source |
|------|-----------|--------|
| `agent_message` | `NormalizedAgentMessage` | Claude/Codex text output |
| `tool_use` | `NormalizedToolUse` | Tool call started |
| `tool_result` | `NormalizedToolResult` | Tool call completed (Codex only) |
| `result` | `NormalizedResult` | Turn/session completed |
| `progress` | raw passthrough `map[string]any` | Claude progress events |
| `system` | `{subtype: string, ...}` | Stderr, thread_started, agent_error, hook events |
| `error` | `{message: string}` or `{message: string, code: int}` | Errors from any source |
| `exit` | `{code: int, error_detail: string}` | Agent process exited |

### System Event Subtypes

| Subtype | Source | Description |
|---------|--------|-------------|
| `stderr` | Both | Stderr line from agent process |
| `stdout_raw` | Claude | Non-JSON line from stdout |
| `thread_started` | Codex | Thread created |
| `agent_error` | ws.go exitLoop | Non-zero exit before `exit` event |
| `hook_*` | Claude | Hook-related system events (stripped to subtype only) |

---

## 9. Steering Flow

### Claude

```
Client sends: {"type": "steer", "data": {"content": "new direction"}}
  → backend.SendSteer(content)
    → SendInterrupt()
      → stdin: {"type":"control_request","request":{"subtype":"interrupt"}}
    → SendPrompt(content)
      → stdin: {"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}
```

Interrupt + new prompt are sent sequentially on the same stdin pipe.

### Codex

```
Client sends: {"type": "steer", "data": {"content": "new direction"}}
  → backend.SendSteer(content)
    → JSON-RPC: turn/steer {threadId, input, expectedTurnId}
```

Requires an active turn (`activeTurnID` must be set). Returns `errCodexNoActiveTurn` if no turn is active.

### Interrupt Only

```
Client sends: {"type": "interrupt"}
  → Claude: control_request.interrupt via stdin
  → Codex: turn/interrupt JSON-RPC
```

---

## 10. Session Resume

Controlled via `AgentConfig.ResumeSession` (from `AGENT_CONFIG` env var).

- **Claude**: Maps to `--resume --session-id <id>` flags. The session ID is set on the backend at construction time. If `ResumeSession` is provided, it replaces the generated UUID.
- **Codex**: Would map to a thread ID, but the current code generates a fresh UUID for `sessionID` unconditionally. Thread reuse would require passing the thread ID to `ensureThread()`.

> **Note**: The current code in `newBackend()` does not thread `AgentConfig` into the backend constructors. This appears to be a gap — `AgentConfig` is parsed but not applied to spawn flags.

---

## 11. Generic Backend (`generic.go`)

Fallback for any agent binary that isn't Claude or Codex.

### Behavior

- Spawns the command with `buildCleanEnv(nil)`.
- Reads stdout and stderr as raw text lines.
- Emits events with type `"stdout"` or `"stderr"` (note: NOT `"agent_message"`).
- If `AGENT_PROMPT` is set: writes prompt to stdin, then closes stdin.
- `SendSteer()` is aliased to `SendPrompt()` (just writes to stdin).
- `SendInterrupt()` sends `SIGINT`, falls back to `SIGKILL`.
- `SendContext()` and `SendMention()` return errors ("not implemented").

### Event Types

| Type | Data |
|------|------|
| `stdout` | `{text: "<line>"}` |
| `stderr` | `{text: "<line>"}` |
| `error` | `{message: "<error>"}` |

These events are **not normalized** (no `agent_message` etc.).

---

## 12. Legacy PTY Sidecar (`legacy_v1.go`)

Activated when `AGENT_CMD` is absent but `AGENT_BIN` is set. Uses a completely different protocol.

### Behavior

- Spawns via PTY (50 rows × 200 cols) for interactive agents, or pipes for prompt mode.
- Raw byte stream stored in a ReplayBuffer.
- Uses the **daemon bridge protocol** (`pkg/bridge` frames), not the v2 event protocol.

### WebSocket Protocol (Legacy)

**Client → Sidecar:**
| Frame type | Data | Action |
|-----------|------|--------|
| `stdin` | raw text | Write to agent stdin |
| `ping` | — | Reply with `pong` |
| `resize` | — | Not implemented |

**Sidecar → Client:**
| Frame type | Fields | Description |
|-----------|--------|-------------|
| `connected` | `session_id`, `mode: "pipe"` | Connection established |
| `stdout` | `data` (UTF-8 or base64), `offset` | Raw agent output |
| `replay` | `data` (base64), `offset` | Catch-up data on reconnect |
| `exit` | `exit_code` | Agent process exited |
| `pong` | — | Ping response |
| `error` | `error: string` | Error message |

### Key Differences from v2

- No event normalization.
- No structured events (agent_message, tool_use, etc.).
- Raw terminal output (PTY bytes).
- Different WebSocket frame schema (bridge frames, not Event objects).
- No health endpoint JSON body differences, but simpler state tracking.

---

## Appendix A: Backend Interface

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

## Appendix B: Feature Support Matrix

| Feature | Claude Prompt | Claude Interactive | Codex Exec | Codex App-Server | Generic |
|---------|:---:|:---:|:---:|:---:|:---:|
| Structured events | ✓ | ✓ | ✓ | ✓ | ✗ |
| Streaming deltas | ✓ | ✓ | ✗ | ✓ | ✗ |
| Interactive prompt | ✗ | ✓ | ✗ | ✓ | ✓ (raw stdin) |
| Steering | ✗ | ✓ | ✗ | ✓ | ✓ (raw stdin) |
| Interrupt | ✗ | ✓ | ✗ | ✓ | ✓ (SIGINT) |
| Context injection | ✗ | ✓ (MCP) | ✗ | ✗ | ✗ |
| @mentions | ✗ | ✓ (MCP) | ✗ | ✗ | ✗ |
| MCP server | ✗ | ✓ | ✗ | ✗ | ✗ |
| Tool approval | auto (skip) | auto (skip) | — | auto (accept) | — |
| Normalization | ✓ | ✓ | ✓ | ✓ | ✗ |
| Session resume | via session-id | via session-id | ✗ | via thread | ✗ |
| Cost tracking | ✓ | ✓ | ✗ | ✗ | ✗ |
| Usage/tokens | ✓ | ✓ | ✗ | ✓ | ✗ |
