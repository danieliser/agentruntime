# Claude Code IDE WebSocket Protocol — Research Document

**Date:** 2026-03-17
**Researcher:** agentruntime team
**Sources:** VS Code extension v2.1.42 (decompiled), claudecode.nvim PROTOCOL.md & STORY.md,
PAOP `rnd/ide-websocket-prototypes` branch, Claude Code CLI `--help`

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Two Distinct Communication Channels](#2-two-distinct-communication-channels)
3. [Channel A: IDE MCP WebSocket (Context Injection)](#3-channel-a-ide-mcp-websocket)
4. [Channel B: stream-json stdio (Agent Output)](#4-channel-b-stream-json-stdio)
5. [CLI Flags for IDE Integration](#5-cli-flags-for-ide-integration)
6. [JSONL Session File Format](#6-jsonl-session-file-format)
7. [Steering & Control Requests](#7-steering--control-requests)
8. [Idle / Disconnect / Reconnect Behavior](#8-idle--disconnect--reconnect-behavior)
9. [PAOP Prototype Findings](#9-paop-prototype-findings)
10. [Known Gaps & Open Questions](#10-known-gaps--open-questions)
11. [Implications for agentruntime](#11-implications-for-agentruntime)

---

## 1. Architecture Overview

Claude Code's IDE integration uses **two independent communication channels** running
simultaneously, not a single bidirectional WebSocket:

```
┌─────────────────────────────────────────────────────┐
│                    VS Code Extension                 │
│                                                      │
│  ┌──────────────┐          ┌───────────────────┐    │
│  │  MCP WS      │◄────────│  Claude CLI        │    │
│  │  Server       │ JSON-RPC│  (child process)   │    │
│  │  (port N)     │────────►│                    │    │
│  │               │  tools  │  stdin ◄── stream- │    │
│  │  Lock file:   │         │  stdout ──► json   │    │
│  │  ~/.claude/   │         │                    │    │
│  │  ide/N.lock   │         └───────────────────┘    │
│  └──────────────┘               │    │              │
│                            NDJSON│    │stream-json   │
│  ┌──────────────┐               │    │              │
│  │  Webview UI   │◄─────────────┘    │              │
│  │  (renders     │◄──────────────────┘              │
│  │   responses)  │                                   │
│  └──────────────┘                                    │
└─────────────────────────────────────────────────────┘
```

**Channel A** — IDE MCP WebSocket: Context injection (selections, files) and IDE tool
calls (openFile, getDiagnostics, openDiff). JSON-RPC 2.0 over WebSocket. IDE is
the server; Claude connects as client.

**Channel B** — stream-json over stdio: All agent output (assistant messages, tool
calls, results, progress). NDJSON lines on stdout; NDJSON lines on stdin for input.
This is how VS Code actually receives Claude's text responses.

---

## 2. Two Distinct Communication Channels

This is the single most important finding. The IDE WebSocket is **not** how Claude's
text responses reach the IDE. The WebSocket carries only MCP tool calls and context
notifications. The actual assistant output flows through the CLI's `--output-format
stream-json` mode over stdout.

Evidence from the extension source (extension.js, spawning code):

```javascript
// The SDK always spawns with stream-json on both sides
m = ["--output-format", "stream-json", "--verbose",
     "--input-format", "stream-json"];
```

The extension reads Claude's stdout as NDJSON lines and dispatches them by `type` field.

---

## 3. Channel A: IDE MCP WebSocket

### 3.1 Discovery & Connection

1. **IDE creates WebSocket server** on a random port (10000–65535), bound to localhost only.
2. **Lock file** written to `~/.claude/ide/{port}.lock`:

```json
{
  "pid": 12345,
  "workspaceFolders": ["/path/to/project"],
  "ideName": "VS Code",
  "transport": "ws",
  "authToken": "550e8400-e29b-41d4-a716-446655440000"
}
```

3. **Environment variables** set on the spawned Claude process:
   - `CLAUDE_CODE_SSE_PORT={port}` — the WebSocket server port
   - `ENABLE_IDE_INTEGRATION=true` — enables IDE features in Claude

4. **Claude connects** to `ws://127.0.0.1:{port}` with auth header:
   ```
   x-claude-code-ide-authorization: {authToken}
   ```

5. **Handshake** — Claude sends JSON-RPC `initialize` request:
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": { "protocolVersion": "2025-03-26" }
}
```

Server responds with capabilities and tool list.

### 3.2 IDE → Claude Notifications

**`selection_changed`** — Sent when user's text selection changes:
```json
{
  "jsonrpc": "2.0",
  "method": "selection_changed",
  "params": {
    "text": "selected text content",
    "filePath": "/absolute/path/to/file.js",
    "fileUrl": "file:///absolute/path/to/file.js",
    "selection": {
      "start": { "line": 10, "character": 5 },
      "end": { "line": 15, "character": 20 },
      "isEmpty": false
    }
  }
}
```

**`at_mentioned`** — When user explicitly sends file context via @-mention:
```json
{
  "jsonrpc": "2.0",
  "method": "at_mentioned",
  "params": {
    "filePath": "/path/to/file",
    "lineStart": 10,
    "lineEnd": 20
  }
}
```

### 3.3 Claude → IDE Tool Calls (MCP)

Claude calls IDE tools via standard MCP `tools/call` requests:

```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "method": "tools/call",
  "params": {
    "name": "openFile",
    "arguments": { "filePath": "/src/main.ts", "preview": true }
  }
}
```

Response follows MCP format:
```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "result": {
    "content": [{ "type": "text", "text": "{\"success\": true}" }]
  }
}
```

### 3.4 Available IDE Tools (12 total)

| Tool | Purpose | Blocking? |
|------|---------|-----------|
| `openFile` | Open file, optionally select text range | No |
| `openDiff` | Show diff view for file changes | **Yes** (waits for accept/reject) |
| `getCurrentSelection` | Get active text selection | No |
| `getLatestSelection` | Get most recent selection (even if not active) | No |
| `getOpenEditors` | List open editor tabs | No |
| `getWorkspaceFolders` | Get workspace folder paths | No |
| `getDiagnostics` | Get language server diagnostics | No |
| `checkDocumentDirty` | Check for unsaved changes | No |
| `saveDocument` | Save a document | No |
| `close_tab` | Close a specific tab | No |
| `closeAllDiffTabs` | Close all diff tabs | No |
| `executeCode` | Execute Python in Jupyter kernel | No |

Note: `openDiff` returns `"FILE_SAVED"` or `"DIFF_REJECTED"` — this is the primary
mechanism for accepting/rejecting file edits in VS Code.

---

## 4. Channel B: stream-json stdio (Agent Output)

This is the channel through which VS Code receives Claude's actual responses. The
extension spawns the Claude CLI with `--output-format stream-json --input-format
stream-json` and reads NDJSON lines from stdout.

### 4.1 Message Types (stdout → IDE)

From the extension source, these message types are emitted on stdout as NDJSON:

| Type | Description |
|------|-------------|
| `assistant` | Assistant response message (contains `message` with Anthropic API format) |
| `user` | User message echo (when `--replay-user-messages` is set) |
| `system` | System-level messages |
| `result` | Final result — signals conversation turn is complete |
| `progress` | Progress indicator (e.g., tool execution status) |
| `control_request` | Claude requesting permission to use a tool |
| `control_response` | Response to a control request |
| `control_cancel_request` | Cancellation of a pending control request |
| `keep_alive` | Heartbeat (skipped by message processor) |
| `streamlined_text` | Streamlined text output (skipped by main loop) |
| `streamlined_tool_use_summary` | Summary of tool use (skipped by main loop) |

### 4.2 Assistant Message Format

The `assistant` message wraps an Anthropic API message object:

```json
{
  "type": "assistant",
  "message": {
    "id": "msg_...",
    "type": "message",
    "role": "assistant",
    "content": [
      { "type": "text", "text": "Here's the code..." },
      {
        "type": "tool_use",
        "id": "toolu_...",
        "name": "Edit",
        "input": { "file_path": "/src/main.ts", "old_string": "...", "new_string": "..." }
      }
    ],
    "model": "claude-sonnet-4-20250514",
    "stop_reason": "end_turn",
    "usage": { "input_tokens": 1234, "output_tokens": 567 }
  },
  "uuid": "abc-123",
  "parentUuid": "parent-uuid",
  "sessionId": "session-uuid",
  "timestamp": "2026-03-17T00:00:00Z",
  "cwd": "/path/to/project",
  "userType": "unknown",
  "version": "1.0",
  "isSidechain": false,
  "requestId": "req-uuid"
}
```

The `message.content` array uses standard Anthropic API content blocks:
- `{ "type": "text", "text": "..." }` — text response
- `{ "type": "tool_use", "id": "...", "name": "...", "input": {...} }` — tool call
- `{ "type": "thinking", "thinking": "..." }` — extended thinking (when enabled)

### 4.3 User Message Format

```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": [
      { "type": "text", "text": "Please fix the bug in auth.ts" },
      { "type": "tool_result", "tool_use_id": "toolu_...", "content": "..." }
    ]
  },
  "uuid": "user-uuid",
  "parentUuid": null,
  "sessionId": "session-uuid",
  "timestamp": "2026-03-17T00:00:00Z",
  "cwd": "/path/to/project",
  "userType": "unknown",
  "version": "1.0",
  "isSidechain": false
}
```

### 4.4 Result Message

Signals the end of a conversation turn:

```json
{
  "type": "result",
  "subtype": "success",
  "cost_usd": 0.0234,
  "duration_ms": 12345,
  "duration_api_ms": 8000,
  "is_error": false,
  "num_turns": 3,
  "session_id": "session-uuid"
}
```

### 4.5 Control Request (Permission Prompt)

When Claude wants to use a tool that requires permission:

```json
{
  "type": "control_request",
  "request": {
    "request_id": "req-abc",
    "subtype": "can_use_tool",
    "tool_name": "Bash",
    "tool_input": { "command": "npm install" }
  }
}
```

### 4.6 Input Format (stdin ← IDE)

The IDE sends NDJSON lines on stdin. Types include:

**User prompt:**
```json
{
  "type": "user",
  "content": "Fix the authentication bug"
}
```

**Control response (permission grant/deny):**
```json
{
  "type": "control_response",
  "response": {
    "request_id": "req-abc",
    "behavior": "allow"
  }
}
```

**Interrupt:**
```json
{
  "type": "control_request",
  "request": {
    "subtype": "interrupt"
  }
}
```

**Set permission mode:**
```json
{
  "type": "control_request",
  "request": {
    "subtype": "set_permission_mode",
    "mode": "acceptEdits"
  }
}
```

**Set model:**
```json
{
  "type": "control_request",
  "request": {
    "subtype": "set_model",
    "model": "claude-opus-4-6"
  }
}
```

**Set max thinking tokens:**
```json
{
  "type": "control_request",
  "request": {
    "subtype": "set_max_thinking_tokens",
    "max_thinking_tokens": 10000
  }
}
```

**Rewind files:**
```json
{
  "type": "control_request",
  "request": {
    "subtype": "rewind_files",
    "user_message_id": "msg-uuid",
    "dry_run": false
  }
}
```

---

## 5. CLI Flags for IDE Integration

```
--output-format stream-json    # NDJSON streaming output on stdout
--input-format stream-json     # NDJSON streaming input on stdin
--include-partial-messages     # Emit partial chunks as they arrive (for live typing)
--replay-user-messages         # Echo user messages back on stdout
--ide                          # Auto-connect to IDE if exactly one lock file exists
--session-id <uuid>            # Use specific session ID
--permission-mode <mode>       # acceptEdits|bypassPermissions|default|dontAsk|plan|auto
--max-thinking-tokens <n>      # Extended thinking budget
--max-turns <n>                # Limit conversation turns
--max-budget-usd <amount>      # Cost cap
--no-session-persistence       # Ephemeral session (no history saved)
--json-schema <schema>         # Enforce structured output
```

### Environment Variables

| Variable | Purpose |
|----------|---------|
| `CLAUDE_CODE_SSE_PORT` | IDE WebSocket server port |
| `ENABLE_IDE_INTEGRATION` | Enable IDE features in Claude |
| `CLAUDE_CODE_EXIT_AFTER_STOP_DELAY` | Set to `0` to disable auto-exit timer |
| `CLAUDE_CODE_ENTRYPOINT` | Entry point type (e.g., `sdk-cli`) |
| `CLAUDECODE` | General Claude Code flag (`1`) |

### VS Code Extension Settings

| Setting | Purpose |
|---------|---------|
| `claudeCode.useTerminal` | Launch in terminal vs native UI |
| `claudeCode.initialPermissionMode` | Default permission handling |
| `claudeCode.environmentVariables` | Custom env vars for Claude process |
| `claudeCode.claudeProcessWrapper` | Custom wrapper executable |

---

## 6. JSONL Session File Format

Claude persists all messages to `~/.claude/projects/{project-hash}/{session-id}.jsonl`.
Each line is a JSON object with a `type` field:

| Type | Description |
|------|-------------|
| `user` | User message (includes `message`, `uuid`, `parentUuid`, `sessionId`, `timestamp`) |
| `assistant` | Assistant message (same fields + `requestId`) |
| `system` | System messages |
| `attachment` | File attachments |
| `progress` | Progress indicators |
| `summary` | Conversation summary (includes `leafUuid`, `summary`) |
| `custom-title` | User-set conversation title (includes `customTitle`) |
| `teleported-from` | Session resume marker (includes `remoteSessionId`, `branch`, `messageCount`) |
| `teleport-skipped-branch` | Skipped branch marker |
| `file-history-snapshot` | File state snapshot at a message point |

---

## 7. Steering & Control Requests

### 7.1 Mid-Execution Steering

The stream-json input format supports injecting control requests at any time:

1. **Interrupt** — Stop current processing:
   ```json
   { "type": "control_request", "request": { "subtype": "interrupt" } }
   ```

2. **Permission grant** — Allow a pending tool call:
   ```json
   {
     "type": "control_response",
     "response": { "request_id": "req-abc", "behavior": "allow" }
   }
   ```

3. **Permission deny** — Reject a tool call:
   ```json
   {
     "type": "control_response",
     "response": { "request_id": "req-abc", "behavior": "deny", "message": "Not allowed" }
   }
   ```

4. **New user message** — Send a new prompt (starts next turn):
   ```json
   { "type": "user", "content": "Now refactor the auth module" }
   ```

### 7.2 Context Injection via IDE WebSocket

The IDE can inject context at any time via `selection_changed` on the MCP WebSocket.
This doesn't steer the conversation directly but makes the selection available as
context for Claude's next turn.

The `at_mentioned` notification is more explicit — it tells Claude the user is
specifically referencing a file range.

### 7.3 File Updated Notification

The extension emits `file_updated` events when files change:
```json
{
  "type": "file_updated",
  "channelId": "channel-uuid",
  "filePath": "/src/main.ts",
  "oldContent": "...",
  "newContent": "..."
}
```

---

## 8. Idle / Disconnect / Reconnect Behavior

### 8.1 MCP WebSocket Keepalive

- **Ping interval:** Extension-defined (observed ~30s)
- Claude sends `ping` requests; IDE responds with `{ "result": {} }`
- Claude has a **~60s hardcoded idle timeout** in TUI mode that triggers
  disconnect/reconnect cycles — this is expected behavior, not a bug

### 8.2 Reconnection

- When Claude reconnects, it finds the IDE's lock file and re-establishes the
  WebSocket connection with the same auth token
- The IDE server replaces the old WebSocket with the new connection
- Lock files are cleaned up when the IDE process exits (PID check)

### 8.3 stream-json Channel

- The stream-json channel has no reconnect mechanism — it's a subprocess pipe
- If the Claude process exits, a new one must be spawned
- `--resume` flag can be used to continue a previous session
- `--session-id` can specify which session to resume

---

## 9. PAOP Prototype Findings

The PAOP `rnd/ide-websocket-prototypes` branch implemented a functional MCP WebSocket
server that mimics VS Code's IDE role. Key findings:

### 9.1 What Worked

- **MCP WebSocket server** — Full JSON-RPC 2.0 implementation with 16 passing tests
- **Lock file discovery** — Claude successfully connects when lock file is present
- **Tool advertising** — Claude discovers and calls IDE tools
- **Context injection** — `selection_changed` notifications successfully inject context
- **Authentication** — Token-based auth via lock file works correctly
- **Keepalive** — 30s ping/pong prevents idle disconnection

### 9.2 What Didn't Work — The Output Formatting Blocker

The prototype assumed the WebSocket would carry Claude's text responses. **It doesn't.**

The IDE WebSocket only carries:
- MCP tool calls FROM Claude (openFile, getDiagnostics, etc.)
- Context notifications TO Claude (selection_changed, at_mentioned)

Claude's actual text output goes to:
- stdout (stream-json NDJSON lines)
- JSONL session file (`~/.claude/sessions/{id}.jsonl`)

This means the `SessionBackend.send_streaming()` interface cannot be satisfied by the
WebSocket alone. The prototype returns placeholder text:

```python
async def send_streaming(self, message, channel_context=""):
    """IDE mode doesn't have a native streaming response path yet.
    The WebSocket carries MCP tool calls (Claude → IDE), not text output.
    Text output goes to the JSONL session file and stdout."""
    yield ParsedEvent(
        type="text",
        text="[IDE mode: context injected, response capture in progress]",
    )
```

### 9.3 Recommended Fix (from prototype comments)

Three approaches were considered:

1. **JSONL tailing** — Monitor `~/.claude/sessions/{id}.jsonl` in parallel with WS
   - Pro: Gets actual Claude output
   - Con: Depends on undocumented file format, session management quirks

2. **Subprocess stdout capture** — Capture stream-json output directly
   - Pro: Direct, structured access to all message types
   - Con: Can't distinguish responses from logs easily; but with `--output-format
     stream-json`, output is well-structured NDJSON

3. **Custom response channel** — New WS message type for responses
   - Pro: Clean bidirectional
   - Con: Requires modifying Claude Code's protocol (not feasible)

**The correct approach is #2** — spawn Claude as a subprocess with `--output-format
stream-json --input-format stream-json` and process NDJSON on stdout/stdin. The MCP
WebSocket is supplementary for IDE tool calls, not primary for output.

---

## 10. Known Gaps & Open Questions

### 10.1 Confirmed Gaps

1. **No streaming delta events** — `--include-partial-messages` is mentioned in the CLI
   help but the exact format of partial messages is not documented. The extension uses
   it (based on `includePartialMessages: !vscode.env.remoteName` — disabled for remote).

2. **Thinking block format** — When extended thinking is enabled, the assistant message
   `content` array includes `{ "type": "thinking", "thinking": "..." }` blocks, but
   the streaming behavior (partial thinking updates) is undocumented.

3. **`streamlined_text` and `streamlined_tool_use_summary`** — These message types
   exist in the stream-json output but are explicitly skipped by the extension's message
   processor. Their exact format and purpose is unclear — possibly a compact format for
   TUI rendering.

4. **Control request schema** — The full set of `subtype` values for control requests
   is not enumerated. Confirmed subtypes: `can_use_tool`, `interrupt`, `set_permission_mode`,
   `set_model`, `set_max_thinking_tokens`, `rewind_files`.

5. **Error message format** — How errors are reported on the stream-json channel is not
   well documented. The `result` type with `is_error: true` handles end-of-turn errors.

### 10.2 Open Questions

1. **Partial message format** — What does `--include-partial-messages` actually emit?
   Are they the same `assistant` type with incomplete content? Or a separate type?

2. **Tool execution progress** — How does the `progress` type work? Is it emitted
   during tool execution with intermediate status?

3. **Multi-turn stdin protocol** — Can new `user` messages be sent on stdin while Claude
   is still processing? Or must you wait for a `result` message first?

4. **Sidechain messages** — The `isSidechain: true` flag appears in session files. What
   triggers sidechain processing and how does it affect output?

5. **WebSocket MCP version** — The protocol references MCP spec "2025-03-26" and the
   code also references "2025-11-25" and "2025-06-18". Which version is authoritative?

---

## 11. Implications for agentruntime

### 11.1 Current agentruntime Architecture

agentruntime currently uses a single WebSocket channel that bridges stdio (stdout/stderr)
from the agent process. This is essentially a raw byte stream wrapped in JSON frames:

```go
// pkg/bridge/frames.go
type ServerFrame struct {
    Type      string  // "stdout", "stderr", "exit", "replay", "connected", "pong", "error"
    Data      string  // Raw output data
    ExitCode  *int
    Offset    int64
    SessionID string
    Mode      string  // "pipe" or "pty"
    Error     string
}
```

### 11.2 What agentruntime Should Add

To properly support IDE-style integration, agentruntime should:

1. **Parse stream-json output** — When the agent is Claude, spawn with `--output-format
   stream-json` and parse NDJSON lines from stdout. Emit structured message frames
   instead of raw stdout bytes:

   ```go
   type AgentMessage struct {
       Type      string          `json:"type"`      // "assistant", "user", "result", etc.
       Message   json.RawMessage `json:"message"`   // Anthropic API message object
       UUID      string          `json:"uuid"`
       SessionID string          `json:"sessionId"`
       // ... other fields
   }
   ```

2. **Implement IDE MCP server** — Optionally run a WebSocket MCP server for IDE tool
   calls. This enables richer integration but is NOT required for basic output streaming.

3. **Support control requests** — Forward control_request/control_response frames
   between the WS client and Claude's stdin, enabling permission management and
   model switching from the client.

4. **Handle the `result` message** — Use the `result` message type as the definitive
   signal that a conversation turn is complete (not process exit).

5. **Implement `--include-partial-messages`** — For real-time streaming to clients,
   enable partial messages and forward them as they arrive.

### 11.3 Architecture Recommendation

```
                    agentruntime
                         │
          ┌──────────────┼──────────────┐
          │              │              │
    WS Client      HTTP Client    IDE MCP Server
    (stream)       (REST API)     (optional)
          │              │              │
          └──────────────┼──────────────┘
                         │
                   Session Manager
                         │
                    Agent Process
                    (claude CLI)
                         │
              ┌──────────┴──────────┐
              │                     │
         stdin (NDJSON)       stdout (NDJSON)
         stream-json in       stream-json out
              │                     │
         user messages         assistant msgs
         control responses     control requests
         interrupts            results
                               progress
```

The key insight is that agentruntime's WebSocket bridge should **parse and re-emit
structured messages** from the stream-json output, rather than forwarding raw bytes.
This enables clients to:
- Render assistant text incrementally
- Handle tool calls with permission prompts
- Track conversation state (turns, costs, errors)
- Implement proper result/completion detection

---

## Appendix A: Lock File Locations

- Default: `~/.claude/ide/{port}.lock`
- Override: `$CLAUDE_CONFIG_DIR/ide/{port}.lock`

## Appendix B: MCP Protocol Version History

- `2024-10-07` — Initial MCP spec
- `2024-11-05` — Updates
- `2025-03-26` — Version referenced in PROTOCOL.md
- `2025-06-18` — Intermediate version
- `2025-11-25` — Latest version found in extension source (`GE` constant)

## Appendix C: VS Code Extension Commands

| Command | Description |
|---------|-------------|
| `claude-vscode.editor.open` | Open Claude in new editor tab |
| `claude-vscode.sidebar.open` | Open Claude in sidebar |
| `claude-vscode.window.open` | Open Claude in new window |
| `claude-vscode.focus` | Focus Claude panel |
| `claude-vscode.blur` | Unfocus Claude panel |
| `claude-vscode.acceptProposedDiff` | Accept diff changes |
| `claude-vscode.rejectProposedDiff` | Reject diff changes |
| `claude-vscode.terminal.open` | Open Claude in terminal |

## Appendix D: claudecode.nvim Reference

The Neovim plugin at `github.com/coder/claudecode.nvim` is a pure-Lua implementation
of the IDE MCP server side. It implements RFC 6455 WebSocket framing, SHA-1 hashing,
and base64 encoding without external dependencies. It provides 100% protocol
compatibility with the official VS Code extension's IDE integration.

Key implementation notes:
- Uses `vim.loop` for async I/O
- Implements all 12 IDE tools
- Supports the same lock file discovery mechanism
- Adds Neovim-specific features (diff accept/reject, terminal management)
- Available commands: `:ClaudeCode`, `:ClaudeCodeSend`, `:ClaudeCodeAdd`, etc.

The PROTOCOL.md from this repo was the primary reference for reverse-engineering the
IDE protocol, authored by tracing the VS Code extension's minified JavaScript using
AST-grep and semantic variable renaming.
