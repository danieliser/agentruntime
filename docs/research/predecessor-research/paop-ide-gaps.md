> **Context:** This document researches the PAOP (Persistence) project's IDE WebSocket prototype, a predecessor approach. The recommendations in this doc were adopted and implemented in agentruntime's sidecar architecture.

# PAOP IDE WebSocket Prototype: Gap Analysis

**Research Date:** 2026-03-17
**Branch:** `rnd/ide-websocket-prototypes` (persistence repo)
**Key Commits:** c3c7335, 4bf33cc, 30e9290, f167a4f, f731b1c
**Status:** Prototype abandoned — response capture unsolved

---

## Executive Summary

PAOP built a fully functional WebSocket MCP server that impersonates a VS Code IDE extension. Claude Code **does connect** to it successfully, and the MCP handshake completes. Tool calls flow bidirectionally. However, the prototype hit an architectural wall: **Claude's text responses do not flow through the IDE WebSocket**. The WebSocket carries only MCP tool requests/responses and context notifications. Actual text output goes to stdout/JSONL files via a separate channel. PAOP never solved the response capture problem and pivoted away from the IDE approach entirely.

---

## What Was Built

### 1. IDEServer (`paop/bridge/ide_server.py`) — 707 lines

A complete MCP JSON-RPC 2.0 WebSocket server implementing the full VS Code IDE extension protocol:

- **Lock file management**: Writes `~/.claude/ide/{port}.lock` with auth token, workspace folders, IDE name
- **Environment variables**: Sets `CLAUDE_CODE_SSE_PORT`, `ENABLE_IDE_INTEGRATION=true`, `CLAUDE_CODE_EXIT_AFTER_STOP_DELAY=0`
- **Auth**: UUID4 token validated via `x-claude-code-ide-authorization` header
- **MCP handshake**: Handles `initialize`, `notifications/initialized`, `ping`
- **11 tool implementations**: `openFile`, `openDiff`, `getCurrentSelection`, `getLatestSelection`, `getOpenEditors`, `getWorkspaceFolders`, `getDiagnostics`, `checkDocumentDirty`, `saveDocument`, `close_tab`, `closeAllDiffTabs`
- **Connection replacement**: On reconnect, closes old transport and accepts new (matches VS Code behavior)
- **30s ping/pong keepalive**: Prevents Claude from assuming the IDE is dead
- **Outbound messaging**: `send_notification()`, `send_request()`, `send_selection()`, `send_at_mention()`

### 2. IDESession (`paop/bridge/ide_server.py`, lines 611-707)

High-level wrapper that:
- Starts the IDEServer
- Spawns `claude --ide` as a subprocess
- Waits for WebSocket connection
- Provides `inject_context()` and `mention_file()` methods

### 3. IDESessionBackend (`paop/bridge/backend_ide.py`) — 194 lines

SessionBackend implementation wrapping IDESession:
- `ensure_running()`: Starts IDE server + spawns Claude
- `send()`: Injects context via `selection_changed` — **returns placeholder, not real response**
- `send_streaming()`: Same — yields a status event, no real streaming
- Implements the SessionBackend ABC alongside CLI and SDK backends

### 4. SDKSessionBackend (`paop/bridge/backend_sdk.py`) — 163 lines

Alternative backend using `claude-agent-sdk` (ClaudeSDKClient):
- Persistent subprocess via JSON-RPC over stdin/stdout
- Actually works for send/receive — `query()` + `receive_response()` loop
- **Was briefly deployed as default** (commit f167a4f), then **reverted** (commit f731b1c) because "SDK backend hits API quota — cli backend uses subscription auth"

### 5. Test Suite (`tests/test_ide_server.py`) — 16 tests

Unit tests covering: initialization, port binding, lock file content, tool list, MCP handshake (`initialize`, `ping`, `tools/list`, `tools/call`), notification sending, request/response round-trip, disconnect behavior, callback system.

### 6. Prototype Script (`research/prototypes/test_ide_spawn.py`)

Live integration test that:
1. Starts IDEServer on ports 19200-19300
2. Spawns `claude --ide` with PTY (to simulate TUI mode)
3. Uses `--mcp-config` with empty config to isolate from external MCP servers
4. Uses `--model haiku --effort low` to minimize cost
5. Tests three things:
   - **Test 1**: `selection_changed` notification (context injection only)
   - **Test 2**: Typing into PTY stdin + CR (simulating user input)
   - **Test 3**: Second prompt via PTY (multi-turn)
6. Monitors WebSocket connection stability for 90s

### 7. Research Documents

- **`ide-response-channel-2026-03-09.md`**: Definitive research on the two-channel architecture
- **`references/vscode-ide-server-annotated.js`**: Reverse-engineered VS Code extension WebSocket server code
- **`references/claudecode.nvim/PROTOCOL.md`**: Full protocol documentation from the Neovim plugin

---

## What Worked

1. **WebSocket connection**: Claude Code CLI (`claude --ide`) **does connect** to the PERSIST IDE server. The `wait_for_connection()` succeeds.

2. **MCP handshake**: The `initialize` exchange completes. Claude sends `initialize`, PERSIST responds with capabilities, Claude sends `notifications/initialized`.

3. **Tool calls flow bidirectionally**: Claude can call `getWorkspaceFolders`, `openFile`, `getDiagnostics`, etc. and PERSIST responds correctly. The tool results arrive in proper MCP format: `{ content: [{ type: "text", text: "..." }] }`.

4. **Context injection**: `selection_changed` and `at_mentioned` notifications are delivered to Claude over WebSocket. Claude receives them.

5. **Lock file discovery**: The `~/.claude/ide/{port}.lock` pattern works correctly — Claude reads it and connects.

6. **Connection lifecycle**: Reconnection (replace old transport) works. Ping/pong keepalive prevents premature disconnection. `EXIT_AFTER_STOP_DELAY=0` keeps the process alive.

7. **`ide_connected` event**: Claude sends `ide_connected` with its PID after connecting, confirming it recognizes the IDE server.

---

## Where It Got Stuck — The Response Capture Problem

### The Core Issue

**Claude's text responses do NOT flow through the IDE WebSocket.**

The IDE protocol is a two-channel architecture:

| Channel | Transport | Direction | Content |
|---------|-----------|-----------|---------|
| IDE MCP | WebSocket | Bidirectional | Tool calls, tool results, context notifications |
| Text output | stdout/JSONL/SSE | Unidirectional (Claude→consumer) | Assistant text, thinking, usage |

The WebSocket carries **only** MCP messages (tool requests, tool responses, IDE notifications). The actual text that Claude generates — the response to prompts — goes to:
- **stdout** (when run as `claude -p`)
- **JSONL session file** (`~/.claude/projects/{key}/{session-uuid}.jsonl`)
- **TUI rendering** (when run as `claude --ide` in interactive mode)

This is by design. VS Code doesn't receive text responses over the WebSocket either. It renders Claude's output through its own chat panel, which consumes a separate streaming channel.

### Evidence from the Code

The `backend_ide.py` `send()` method explicitly acknowledges the gap:

```python
async def send(self, message: str) -> str:
    # ...
    await self._session.inject_context(message)
    # The IDE protocol is primarily for context injection, not
    # request-response. Full response capture requires monitoring
    # the JSONL session files or implementing a response channel.
    return "[IDE mode: context injected — response capture pending]"
```

And `send_streaming()`:

```python
# IDE mode doesn't have a native streaming response path yet.
# The WebSocket carries MCP tool calls (Claude → IDE), not text output.
# Text output goes to the JSONL session file and stdout.
```

### What Was NOT Tried (Potential Solutions)

The research doc (`ide-response-channel-2026-03-09.md`) identified but did not implement:

1. **JSONL tailing**: Read `~/.claude/projects/{key}/{session-uuid}.jsonl` in real-time, parse streaming events. This is what the web UI eventually did (commit 3e074c2).

2. **stdout piping**: Capture the `claude --ide` subprocess stdout. The prototype spawns Claude with `stdout=slave_fd` (PTY), so output goes to the PTY — but the test script reads it as raw terminal output, not structured events.

3. **SSE endpoint on IDEServer**: The research doc recommended adding `GET /ide/sse` that forwards Claude's streaming events. Never implemented.

4. **Dual-channel approach**: WebSocket for tools + JSONL tailing for text. The research doc recommended this as the architecture. Never implemented.

---

## The `claude --ide` Flag

### What It Does

`claude --ide` puts Claude Code into "IDE mode":
- Connects to the WebSocket server specified by `CLAUDE_CODE_SSE_PORT`
- Authenticates using the token from the lock file
- Registers MCP tools from the IDE
- Runs in TUI mode (interactive terminal) but with IDE integration active
- Sends `ide_connected` notification with its PID
- Periodically reconnects if the WebSocket drops (~60s idle timeout)

### What It Does NOT Do

- Does NOT stream text responses over the WebSocket
- Does NOT provide a programmatic way to send user prompts (the PTY approach in the prototype is a hack)
- Does NOT expose an API for response capture (you must read stdout, JSONL, or tmux pane)

### The PTY Approach

The prototype tried sending prompts by writing to the PTY:
```python
os.write(master_fd, prompt.encode())
os.write(master_fd, b"\r")
```

This simulates typing into the TUI. It works for sending prompts, but capturing the response requires reading the PTY output — which is raw terminal escape sequences, not structured data.

---

## Comparison with claudecode.nvim

| Aspect | claudecode.nvim | PAOP IDEServer |
|--------|----------------|----------------|
| WebSocket server | Pure Lua, RFC 6455 | Python aiohttp |
| Discovery | Lock file + env vars | Same (compatible) |
| Auth | UUID4 token via header | Same (compatible) |
| MCP tools | 10 tools (VS Code parity) | 11 tools |
| Text response capture | **Not their problem** — Neovim terminal renders TUI | **The blocker** — no terminal, need structured output |
| Keepalive | 30s ping | 30s ping (copied from nvim) |
| Reconnect | Accept new, close old | Same pattern |
| User input | Neovim terminal (native TUI) | PTY hack / no clean solution |

**The fundamental difference**: claudecode.nvim doesn't need to capture responses programmatically. It runs Claude in a Neovim terminal buffer — the user sees the TUI directly. The WebSocket is just for tool support and context injection. Claude's text output goes to the terminal, which the user reads.

PAOP's use case is different: it needs to **capture Claude's text responses** and relay them to external platforms (Telegram, Slack, web UI). The IDE protocol was never designed for this.

---

## The SDK Backend Detour

PAOP briefly tried the Agent SDK (`claude-agent-sdk`) as an alternative:

- **Commit f167a4f**: Switched default backend from CLI to SDK
- **Why**: SDK uses stdin/stdout JSON-RPC — proper request/response cycle, `query()` → `receive_response()` pattern, typed Python API
- **Commit f731b1c**: Reverted to CLI backend
- **Why reverted**: "SDK backend hits API quota — cli backend uses subscription auth." The SDK uses API keys (per-token billing), while CLI uses OAuth subscription (unlimited). For a personal daemon running continuously, API key billing was prohibitive.

This is a critical finding: **the only backend that cleanly solves request/response (SDK) requires API key auth, which is expensive. The only auth model that's affordable (CLI/subscription) doesn't provide clean response capture.**

---

## After the Prototype: What PAOP Actually Shipped

After abandoning the IDE approach, PAOP's bridge system evolved to:

1. **CLI backend** (`backend_cli.py`): Default. Spawns `claude -p --output-format stream-json` per message. Parses JSONL from stdout via `StreamParser`. Works but high latency (process spawn per message).

2. **JSONL session reading**: For the web UI, reads `~/.claude/projects/{key}/{session-uuid}.jsonl` with incremental polling (commit 3e074c2). This is the response capture approach that the IDE prototype recommended but never implemented.

3. **Provider abstraction**: Refactored the API executor into provider sessions (`769b33e`, `e3d00b8`, `c0506bc`) — Anthropic provider + OpenAI provider. This is for SDK-mode execution, not bridge sessions.

---

## Recommendations for AgentRuntime

### 1. Don't Repeat the IDE Approach for Response Capture

The IDE WebSocket protocol is designed for **tool support and context injection**, not response streaming. If you need to capture Claude's text responses programmatically, use:

- **`claude -p --output-format stream-json`** — one-shot, structured JSONL on stdout
- **Agent SDK** — persistent process, typed API, but requires API key auth
- **JSONL tailing** — read session files in real-time (brittle, path resolution is complex)

### 2. The IDE Protocol IS Useful For

- **Context injection**: `selection_changed`, `at_mentioned` — push relevant context to Claude during execution
- **Tool extension**: Register custom MCP tools that Claude can call during work
- **Workspace awareness**: `getWorkspaceFolders`, `getDiagnostics` — give Claude IDE-level context
- **Diff management**: `openDiff`, `closeAllDiffTabs` — integrate with file editing UI

If AgentRuntime needs these capabilities alongside response capture, the dual-channel approach (WebSocket for tools + stdout/JSONL for text) is the path.

### 3. Auth Model Matters

- **CLI mode** (`claude`, `claude -p`, `claude --ide`): Uses OAuth subscription auth. Unlimited usage on $20/mo or $200/mo plans. Can't capture responses over WebSocket.
- **SDK mode** (`claude-agent-sdk`): Uses API key auth. Per-token billing. Clean programmatic interface.
- **The gap**: No clean way to get both subscription auth AND programmatic response capture in a single persistent session.

### 4. What's Reusable from PAOP

The `IDEServer` class is well-built and protocol-compliant:
- MCP handshake is correct
- Tool implementations match VS Code extension
- Auth, discovery, keepalive all work
- 16 unit tests pass

If AgentRuntime wants to inject context into running Claude sessions or extend them with custom tools, this code is a solid starting point. Just don't expect text responses to come back over the WebSocket.

### 5. The `--ide` Flag is Not `--sdk`

`claude --ide` is for terminal-based Claude with IDE integration. It's still TUI mode — it expects a human-readable terminal. For programmatic use:

- `claude -p` (print mode) — structured output, no TUI
- `claude -p --output-format stream-json` — JSONL streaming
- Agent SDK — persistent process, JSON-RPC over stdio

### 6. JSONL Path Resolution is Non-Trivial

PAOP's commit 3e074c2 reveals the complexity: named chat sessions store JSONL at `~/.claude/projects/{key}/{claude-session-uuid}.jsonl`, where the UUID is Claude's internal session ID, not PAOP's. Resolving the correct file requires looking up the Claude session UUID from PAOP's own `chat_sessions` table. This mapping is fragile and undocumented.

---

## File Reference

| File | Location | Status |
|------|----------|--------|
| `ide_server.py` | `paop/bridge/ide_server.py` | Production code, ~600 lines |
| `backend_ide.py` | `paop/bridge/backend_ide.py` | Stub — response capture never implemented |
| `backend_sdk.py` | `paop/bridge/backend_sdk.py` | Working but reverted (API key billing) |
| `session_backend.py` | `paop/bridge/session_backend.py` | ABC + factory, supports cli/sdk/ide |
| `test_ide_server.py` | `tests/test_ide_server.py` | 16 unit tests, all passing |
| `test_ide_spawn.py` | `research/prototypes/test_ide_spawn.py` | Live integration test script |
| `ide-response-channel.md` | `research/.../ide-response-channel-2026-03-09.md` | Definitive two-channel research |
| `vscode-ide-server.js` | `research/.../references/vscode-ide-server-annotated.js` | Reverse-engineered VS Code extension |
| `PROTOCOL.md` | `research/.../claudecode.nvim/PROTOCOL.md` | Full protocol documentation |

---

## Timeline

| Date | Event |
|------|-------|
| 2026-03-09 | `c3c7335` — IDEServer WebSocket MCP server built (707 lines, 16 tests) |
| 2026-03-09 | `4bf33cc` — SDK and IDE session backends created |
| 2026-03-09 | Research doc on two-channel architecture completed |
| 2026-03-09 | `f167a4f` — Switched default to SDK backend |
| 2026-03-09 | `f731b1c` — Reverted to CLI backend (API key billing) |
| 2026-03-10 | `30e9290` — Minor refinements to IDE server, but focus shifted to CLI backend |
| 2026-03-10+ | IDE approach abandoned; CLI backend + JSONL tailing became the path forward |
