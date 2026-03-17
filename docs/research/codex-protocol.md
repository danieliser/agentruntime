# Codex App-Server Protocol Research

**Date:** 2026-03-16
**Codex CLI Version:** 0.114.0
**Status:** Experimental (marked `[experimental]` in CLI help)
**Sources:** Generated JSON Schema + TypeScript bindings from local CLI, developers.openai.com/codex/app-server, openai/codex GitHub repo

---

## 1. Overview

Codex CLI exposes a full JSON-RPC 2.0 protocol via its `app-server` subcommand. This is the same protocol used by the VS Code extension and the official TypeScript/Python SDKs. The protocol supports bidirectional communication with threads, turns, streaming events, mid-turn steering, approval callbacks, and session resume.

For agentruntime, this means we have two viable integration paths:
1. **Direct app-server protocol** — Launch `codex app-server` and speak JSON-RPC over stdio or WebSocket
2. **exec mode with JSONL** — Launch `codex exec --json` for simpler one-shot tasks

---

## 2. Transport Layer

### 2.1 Transports

The `--listen` flag controls transport:

| Transport | Flag | Format |
|-----------|------|--------|
| **stdio** (default) | `--listen stdio://` | Newline-delimited JSON (JSONL) on stdin/stdout |
| **WebSocket** | `--listen ws://0.0.0.0:PORT` | One JSON-RPC message per text frame |

WebSocket mode uses bounded queues. Overloaded requests are rejected with error code `-32001` ("Server overloaded; retry later").

### 2.2 Wire Format

Standard JSON-RPC 2.0, but the `"jsonrpc": "2.0"` field is **omitted** on the wire. Four message shapes:

```typescript
// Request (client → server or server → client)
{ method: string, id: string | number, params?: any, trace?: { traceparent?, tracestate? } }

// Notification (no response expected)
{ method: string, params?: any }

// Response
{ id: string | number, result: any }

// Error
{ id: string | number, error: { code: number, message: string, data?: any } }
```

W3C Trace Context (`traceparent`/`tracestate`) is optionally supported on requests for distributed tracing.

---

## 3. Initialization Handshake

Before any other method, the client must complete a two-step handshake:

### Step 1: `initialize` request

```json
{
  "method": "initialize",
  "id": 0,
  "params": {
    "clientInfo": {
      "name": "agentruntime",
      "title": "Agent Runtime Daemon",
      "version": "0.3.0"
    },
    "capabilities": {
      "experimentalApi": true,
      "optOutNotificationMethods": []
    }
  }
}
```

Response:
```json
{ "id": 0, "result": { "userAgent": "codex-cli/0.114.0" } }
```

### Step 2: `initialized` notification

```json
{ "method": "initialized" }
```

Pre-initialization requests are rejected with `"Not initialized"` error.

---

## 4. Core Primitives

| Primitive | Description |
|-----------|-------------|
| **Thread** | A conversation between user and agent. Contains turns. Persisted to `~/.codex/sessions/`. |
| **Turn** | A single user request + agent work cycle. Contains items. |
| **Item** | A unit of I/O: user message, agent message, command execution, file change, MCP tool call, etc. |

---

## 5. Client → Server Methods (Full List)

### 5.1 Thread Management

| Method | Params Type | Purpose |
|--------|-------------|---------|
| `thread/start` | `ThreadStartParams` | Create new conversation |
| `thread/resume` | `ThreadResumeParams` | Reopen existing thread by ID, path, or history |
| `thread/fork` | `ThreadForkParams` | Branch history into new thread |
| `thread/read` | `ThreadReadParams` | Read stored thread without resuming |
| `thread/list` | `ThreadListParams` | Page through threads (filters: archived, cwd, modelProviders, sourceKinds) |
| `thread/loaded/list` | `ThreadLoadedListParams` | List in-memory thread IDs |
| `thread/archive` | `ThreadArchiveParams` | Archive thread |
| `thread/unarchive` | `ThreadUnarchiveParams` | Restore archived thread |
| `thread/unsubscribe` | `ThreadUnsubscribeParams` | Remove connection subscription |
| `thread/compact/start` | `ThreadCompactStartParams` | Trigger context compaction |
| `thread/rollback` | `ThreadRollbackParams` | Drop last N turns |
| `thread/name/set` | `ThreadSetNameParams` | Set thread display name |
| `thread/metadata/update` | `ThreadMetadataUpdateParams` | Update thread metadata |
| `thread/backgroundTerminals/clean` | — | Clean background terminal processes |

### 5.2 Turn Control

| Method | Params Type | Purpose |
|--------|-------------|---------|
| `turn/start` | `TurnStartParams` | Send user input and begin agent generation |
| `turn/steer` | `TurnSteerParams` | Append input to active turn mid-execution |
| `turn/interrupt` | `TurnInterruptParams` | Cancel active turn |

### 5.3 Review

| Method | Params Type | Purpose |
|--------|-------------|---------|
| `review/start` | `ReviewStartParams` | Start code review (uncommittedChanges, baseBranch, commit, custom) |

### 5.4 Configuration & Models

| Method | Params Type | Purpose |
|--------|-------------|---------|
| `model/list` | `ModelListParams` | List available models with capabilities |
| `config/read` | `ConfigReadParams` | Read effective configuration |
| `config/value/write` | `ConfigValueWriteParams` | Write single config key |
| `config/batchWrite` | `ConfigBatchWriteParams` | Atomic config edits |
| `configRequirements/read` | — | Read admin requirements |
| `experimentalFeature/list` | — | List feature flags |
| `collaborationMode/list` | — | List collaboration presets |

### 5.5 Standalone Command Execution

| Method | Params Type | Purpose |
|--------|-------------|---------|
| `command/exec` | `CommandExecParams` | Run command without creating thread |
| `command/exec/write` | `CommandExecWriteParams` | Write stdin to running command |
| `command/exec/terminate` | `CommandExecTerminateParams` | Kill running command |
| `command/exec/resize` | `CommandExecResizeParams` | Resize PTY |

### 5.6 Skills, Plugins, Apps

| Method | Purpose |
|--------|---------|
| `skills/list` | List available skills |
| `skills/config/write` | Enable/disable skills |
| `skills/remote/list` | List remote skills |
| `skills/remote/export` | Export skills |
| `plugin/list` | List plugins |
| `plugin/install` | Install plugin |
| `plugin/uninstall` | Uninstall plugin |
| `app/list` | List connector apps |

### 5.7 Account & Auth

| Method | Purpose |
|--------|---------|
| `account/login/start` | Begin OAuth login |
| `account/login/cancel` | Cancel login |
| `account/logout` | Log out |
| `account/read` | Read account info |
| `account/rateLimits/read` | Read rate limits |
| `getAuthStatus` | Check auth status |

### 5.8 MCP Server Management

| Method | Purpose |
|--------|---------|
| `mcpServer/oauth/login` | Start MCP server OAuth login |
| `config/mcpServer/reload` | Reload MCP config from disk |
| `mcpServerStatus/list` | List MCP servers, tools, resources |

### 5.9 Realtime (Voice/Audio)

| Method | Purpose |
|--------|---------|
| `thread/realtime/start` | Start realtime session |
| `thread/realtime/appendAudio` | Send audio frames |
| `thread/realtime/appendText` | Send text in realtime |
| `thread/realtime/stop` | Stop realtime session |

### 5.10 Utility

| Method | Purpose |
|--------|---------|
| `fuzzyFileSearch` | Single-shot file search |
| `fuzzyFileSearch/session*` | Streaming file search session |
| `getConversationSummary` | Get conversation summary |
| `gitDiffToRemote` | Get git diff to remote |
| `feedback/upload` | Submit feedback |
| `externalAgentConfig/detect` | Detect migratable external agent configs |
| `externalAgentConfig/import` | Import external agent config |

---

## 6. Server → Client Requests (Approval Callbacks)

These are JSON-RPC requests FROM the server that require a client response:

| Method | Params | Purpose |
|--------|--------|---------|
| `item/commandExecution/requestApproval` | `CommandExecutionRequestApprovalParams` | Approve/deny command execution |
| `item/fileChange/requestApproval` | `FileChangeRequestApprovalParams` | Approve/deny file changes |
| `item/permissions/requestApproval` | `PermissionsRequestApprovalParams` | Approve/deny permission grants |
| `item/tool/requestUserInput` | `ToolRequestUserInputParams` | Solicit user input for tool |
| `item/tool/call` | `DynamicToolCallParams` | Execute client-side dynamic tool |
| `mcpServer/elicitation/request` | `McpServerElicitationRequestParams` | MCP elicitation callback |
| `applyPatchApproval` | `ApplyPatchApprovalParams` | Approve patch application (v1) |
| `execCommandApproval` | `ExecCommandApprovalParams` | Approve command execution (v1) |
| `account/chatgptAuthTokens/refresh` | — | Refresh ChatGPT auth tokens |

### Approval Flow

1. Server emits `item/started` with pending item
2. Server sends `item/*/requestApproval` request with `id`
3. Client responds with decision (`accept`, `acceptForSession`, `decline`, `cancel`)
4. Server emits `serverRequest/resolved`
5. Server emits `item/completed` with final status

For agentruntime with `--dangerously-bypass-approvals-and-sandbox`, these callbacks are skipped.

---

## 7. Server → Client Notifications (Streaming Events)

### 7.1 Thread Events

| Notification | Purpose |
|-------------|---------|
| `thread/started` | Thread created/resumed |
| `thread/status/changed` | Runtime status change |
| `thread/archived` | Thread archived |
| `thread/unarchived` | Thread restored |
| `thread/closed` | Thread closed (last subscriber left) |
| `thread/name/updated` | Thread name changed |
| `thread/tokenUsage/updated` | Token usage updated |
| `thread/compacted` | Context compaction completed |

### 7.2 Turn Events

| Notification | Purpose |
|-------------|---------|
| `turn/started` | Turn began (includes `turn_id`, `collaboration_mode_kind`) |
| `turn/completed` | Turn finished (includes status, error, last agent message) |
| `turn/diff/updated` | Aggregated unified diff updated |
| `turn/plan/updated` | Agent plan updated with step status |

### 7.3 Item Events

| Notification | Purpose |
|-------------|---------|
| `item/started` | Item work began |
| `item/completed` | Item finished (authoritative final state) |
| `item/agentMessage/delta` | Streamed agent text |
| `item/plan/delta` | Streamed plan text |
| `item/reasoning/summaryTextDelta` | Readable reasoning delta |
| `item/reasoning/summaryPartAdded` | Reasoning summary part added |
| `item/reasoning/textDelta` | Raw reasoning text delta |
| `item/commandExecution/outputDelta` | Command stdout/stderr delta |
| `item/commandExecution/terminalInteraction` | Terminal interaction event |
| `item/fileChange/outputDelta` | File change output delta |
| `item/mcpToolCall/progress` | MCP tool call progress |

### 7.4 Other Notifications

| Notification | Purpose |
|-------------|---------|
| `serverRequest/resolved` | Pending approval request resolved |
| `error` | Server-side error |
| `model/rerouted` | Model changed mid-session |
| `deprecationNotice` | Deprecation warning |
| `configWarning` | Configuration warning |
| `skills/changed` | Skills list updated |
| `app/list/updated` | App list changed |
| `account/updated` | Account info changed |
| `account/rateLimits/updated` | Rate limits changed |
| `account/login/completed` | Login flow completed |

---

## 8. Key Data Types

### 8.1 ThreadStartParams (Creating a Session)

```typescript
{
  model?: string,              // e.g. "gpt-5.4"
  modelProvider?: string,
  serviceTier?: "default" | "flex",
  cwd?: string,                // Working directory
  approvalPolicy?: "untrusted" | "on-request" | "never",
  sandbox?: "read-only" | "workspace-write" | "danger-full-access",
  config?: Record<string, any>, // Config overrides
  baseInstructions?: string,    // System prompt prepended
  developerInstructions?: string, // Developer-level instructions
  personality?: "friendly" | "concise" | ...,
  ephemeral?: boolean,          // Skip session persistence
  dynamicTools?: DynamicToolSpec[], // Client-provided tools (experimental)
  experimentalRawEvents: boolean,
  persistExtendedHistory: boolean
}
```

### 8.2 TurnStartParams (Sending a Prompt)

```typescript
{
  threadId: string,
  input: UserInput[],           // Array of text, image, skill, mention
  cwd?: string,                 // Override cwd for this turn
  approvalPolicy?: ...,         // Override approval policy
  sandboxPolicy?: ...,          // Override sandbox
  model?: string,               // Override model
  effort?: "low" | "medium" | "high",
  personality?: ...,
  outputSchema?: object,        // JSON Schema for structured output
  collaborationMode?: { kind: string, settings?: { developer_instructions?: string | null } }
}
```

### 8.3 TurnSteerParams (Mid-Turn Steering)

```typescript
{
  threadId: string,
  input: UserInput[],           // Additional input to inject
  expectedTurnId: string        // Must match active turn (precondition)
}
```

### 8.4 UserInput Types

```typescript
type UserInput =
  | { type: "text", text: string, text_elements: TextElement[] }
  | { type: "image", url: string }
  | { type: "localImage", path: string }
  | { type: "skill", name: string, path: string }
  | { type: "mention", name: string, path: string }
```

### 8.5 SandboxPolicy

```typescript
type SandboxPolicy =
  | { type: "dangerFullAccess" }
  | { type: "readOnly" }
  | { type: "workspaceWrite",
      writableRoots: string[],
      readOnlyAccess?: { type: "fullAccess" } | { type: "restricted", readableRoots: string[] },
      networkAccess?: boolean }
  | { type: "externalSandbox",
      networkAccess?: "restricted" | "enabled" }
```

### 8.6 DynamicToolSpec (Client-Provided Tools)

```typescript
{
  name: string,
  description: string,
  inputSchema: object  // JSON Schema
}
```

---

## 9. Exec Mode (Non-Interactive)

For simpler one-shot tasks, `codex exec` provides:

```bash
codex exec --json "Fix the bug" --full-auto --cd /path/to/repo
```

### 9.1 JSONL Event Stream (stdout with `--json`)

Each line is a JSON object with dot-delimited types:
```json
{"type":"thread.started","thread_id":"0199a213-..."}
{"type":"turn.started",...}
{"type":"item.started",...}
{"type":"item.completed",...}
{"type":"turn.completed","usage":{"input_tokens":24763,"output_tokens":122}}
```

### 9.2 Key Exec Flags

| Flag | Purpose |
|------|---------|
| `--json` | JSONL event stream to stdout |
| `--full-auto` | Sandbox + auto-approve (`-a on-request --sandbox workspace-write`) |
| `--dangerously-bypass-approvals-and-sandbox` | No approvals, no sandbox |
| `--ephemeral` | Don't persist session |
| `--output-schema FILE` | JSON Schema for structured output |
| `-o FILE` | Write last agent message to file |
| `-C DIR` | Set working directory |
| `-m MODEL` | Override model |
| `--no-alt-screen` | Inline TUI (preserves scrollback) |
| `--skip-git-repo-check` | Run outside git repo |

### 9.3 Exec Resume

```bash
codex exec resume --last        # Resume most recent session
codex exec resume SESSION_ID    # Resume by ID
```

---

## 10. MCP Server Mode

```bash
codex mcp-server
```

Exposes Codex as a Model Context Protocol server over stdio. This allows other MCP clients to invoke Codex as a tool. Limited to the MCP protocol surface (tools, resources).

---

## 11. IDE Integration (VS Code Extension)

The VS Code extension uses the **same `app-server` protocol** over stdio. Key details:

- Extension spawns `codex app-server` as a subprocess
- Communicates via JSONL over stdin/stdout
- Uses `clientInfo.name = "codex_vscode"` during initialization
- Analytics are enabled by default for the extension (`--analytics-default-enabled`)
- Full bidirectional protocol: streaming events, approval callbacks, steering

This confirms there is **no separate IDE protocol** — it's the same JSON-RPC app-server protocol documented here.

---

## 12. Official SDKs

### 12.1 TypeScript SDK

```bash
npm install @openai/codex-sdk
```

- Wraps the CLI binary, spawning `codex app-server` as a subprocess
- Communicates via JSONL over stdio (not WebSocket)
- Thread-based API: `codex.startThread()`, `codex.resumeThread(id)`
- `thread.run(prompt)` — buffered single-turn execution
- `thread.runStreamed(prompt)` — async generator for real-time events
- Configurable: `workingDirectory`, `env`, `config`, `baseUrl`, `outputSchema`
- Injects `CODEX_API_KEY` automatically

### 12.2 Python SDK

```bash
pip install codex-app-server-sdk
```

- Version 0.2.0 (experimental), requires Python >= 3.10
- Wraps `codex-cli-bin` binary over stdio JSON-RPC
- Pydantic-based wire models (snake_case → camelCase serialization)
- API: `codex = Codex()`, `thread = codex.thread_start(model="...")`, `thread.turn(TextInput(...)).run()`
- Configurable via `AppServerConfig(codex_bin=...)`

### 12.3 Go SDK

**No official Go SDK exists.** The SDK directory contains only TypeScript and Python.

---

## 13. Implications for agentruntime

### 13.1 Recommended Integration Strategy

**For full programmatic control (interactive sessions):**

1. Spawn `codex app-server --listen stdio://` as a subprocess
2. Speak JSONL JSON-RPC over stdin/stdout
3. Complete initialize handshake
4. Use `thread/start` + `turn/start` for sessions
5. Handle approval callbacks with auto-approve responses
6. Stream events via server notifications
7. Use `turn/steer` for mid-execution steering
8. Use `thread/resume` for session recovery

**For one-shot tasks (simpler):**

1. Spawn `codex exec --json --full-auto --cd /path "prompt"`
2. Parse JSONL events from stdout
3. Wait for `turn.completed` event
4. Extract final message

### 13.2 Docker Considerations

In Docker containers, use `--dangerously-bypass-approvals-and-sandbox` since the container itself provides isolation. This eliminates all approval callbacks.

### 13.3 Config Injection

Thread start accepts `config` as flattened key-value pairs matching `config.toml` paths:
```json
{
  "config": {
    "model": "gpt-5.4",
    "sandbox_permissions": ["disk-full-read-access"]
  }
}
```

Additionally, `-c` flag overrides on CLI launch can set defaults.

### 13.4 Steering vs New Turn

- **`turn/start`** — Creates a new turn (user prompt → agent response cycle)
- **`turn/steer`** — Injects additional input into an active turn WITHOUT creating a new turn. Requires `expectedTurnId` as a precondition check.
- **`turn/interrupt`** — Cancels the active turn (sets status to "interrupted")

### 13.5 Session Persistence

Sessions are stored in `~/.codex/sessions/`. The `thread/resume` method can reload by:
1. `threadId` (standard)
2. `history` array (for cloud use — reconstruct from memory)
3. `path` (load from specific rollout file)

### 13.6 Dynamic Tools

With `experimentalApi: true`, clients can inject custom tools via `dynamicTools` on `thread/start`. When the agent invokes them, the server sends `item/tool/call` requests that the client must handle. This enables agentruntime to expose custom capabilities to the Codex agent.

### 13.7 Schema Generation

Codex can auto-generate its own protocol schema:
```bash
codex app-server generate-json-schema --out ./schemas --experimental
codex app-server generate-ts --out ./bindings --experimental
```

This can be used to auto-generate Go client types from the JSON Schema.

---

## 14. Protocol Comparison: Codex vs Claude Code

| Aspect | Codex app-server | Claude Code |
|--------|-----------------|-------------|
| **Protocol** | JSON-RPC 2.0 (explicit) | Proprietary JSONL (no formal spec) |
| **Transport** | stdio or WebSocket | stdio only |
| **Initialization** | `initialize` + `initialized` handshake | None (implicit) |
| **Thread model** | Explicit thread/turn/item hierarchy | Single session, no formal thread concept |
| **Steering** | `turn/steer` with precondition check | Write to stdin (ad-hoc) |
| **Approval** | Bidirectional JSON-RPC requests | Lock-file + inotify pattern |
| **Resume** | `thread/resume` by ID/path/history | `claude --resume` flag |
| **Schema** | Self-documenting (generate-json-schema) | Reverse-engineered |
| **SDKs** | TypeScript + Python (official) | None (community) |
| **MCP server** | `codex mcp-server` (built-in) | `claude mcp-serve-*` (partial) |
| **Structured output** | `--output-schema` / `outputSchema` | Not supported |

---

## 15. Raw Event Type Catalog (EventMsg)

Complete list of event types emitted by the server (v1 event stream, used by `codex exec --json`):

```
error, warning, session_configured, thread_name_updated,
realtime_conversation_started, realtime_conversation_realtime, realtime_conversation_closed,
model_reroute, context_compacted, thread_rolled_back,
task_started, task_complete, token_count,
agent_message, agent_message_delta, agent_message_content_delta,
user_message,
agent_reasoning, agent_reasoning_delta,
agent_reasoning_raw_content, agent_reasoning_raw_content_delta,
agent_reasoning_section_break,
reasoning_content_delta, reasoning_raw_content_delta,
mcp_startup_update, mcp_startup_complete,
mcp_tool_call_begin, mcp_tool_call_end,
web_search_begin, web_search_end,
image_generation_begin, image_generation_end,
exec_command_begin, exec_command_output_delta, exec_command_end,
terminal_interaction,
view_image_tool_call,
exec_approval_request, request_permissions, request_user_input,
dynamic_tool_call_request, dynamic_tool_call_response,
elicitation_request,
apply_patch_approval_request,
background_event,
undo_started, undo_completed,
stream_error,
patch_apply_begin, patch_apply_end,
turn_diff, turn_aborted,
get_history_entry_response,
mcp_list_tools_response,
list_custom_prompts_response, list_skills_response, list_remote_skills_response,
remote_skill_downloaded, skills_update_available,
plan_update, plan_delta,
item_started, item_completed,
hook_started, hook_completed,
entered_review_mode, exited_review_mode,
raw_response_item,
deprecation_notice,
shutdown_complete,
collab_agent_spawn_begin, collab_agent_spawn_end,
collab_agent_interaction_begin, collab_agent_interaction_end,
collab_waiting_begin, collab_waiting_end,
collab_close_begin, collab_close_end,
collab_resume_begin, collab_resume_end
```

---

## 16. Files Generated for Reference

The following were generated locally and can be regenerated at any time:

- `/tmp/codex-schema/` — Full JSON Schema bundle (488KB v1, 418KB v2)
- `/tmp/codex-ts/` — TypeScript type definitions (239 files v1, 326 files v2)

To regenerate:
```bash
codex app-server generate-json-schema --out /tmp/codex-schema --experimental
codex app-server generate-ts --out /tmp/codex-ts --experimental
```
