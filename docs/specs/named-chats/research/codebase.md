# Named Persistent Chat Sessions — Codebase Research

> Generated for spec writing. All file:line references are anchored to the codebase as of this analysis.

---

## Overview

**Language**: Go
**HTTP framework**: Gin (`github.com/gin-gonic/gin`)
**WebSocket**: `github.com/gorilla/websocket`
**CLI framework**: `flag` stdlib (NOT Cobra — `os.Args[1]` dispatch in `cmd/agentd/main.go:25`)
**Data directory**: XDG-compliant `~/.local/share/agentruntime`, overridable via `AGENTRUNTIME_DATA_DIR` env or `--data-dir` flag (`cmd/agentd/main.go:168`)
**Default port**: 8090

### Key Directories

```
cmd/agentd/          — daemon entrypoint + CLI subcommands (dispatch, attach)
cmd/sidecar/         — in-container sidecar (stall detection, event streaming)
pkg/api/             — HTTP server, routes, handlers, types
pkg/api/schema/      — shared request/response types (used across API, SDK, CLI)
pkg/session/         — Session struct, Manager registry, ReplayBuffer, log files
pkg/session/agentsessions/ — per-agent session dir init + resume discovery
pkg/runtime/         — Runtime interface, DockerRuntime, LocalSidecarRuntime
pkg/materialize/     — writes agent config files, builds Docker mounts
pkg/bridge/          — WebSocket bridge (sidecar ↔ daemon)
```

---

## Session System

### Session Struct (`pkg/session/session.go`)

```go
type Session struct {
    ID          string                // UUID
    TaskID      string                // optional
    AgentName   string                // "claude" | "codex"
    RuntimeName string                // "local" | "docker"
    SessionDir  string                // {dataDir}/claude-sessions/{id}/
    VolumeName  string                // "agentruntime-vol-{id}" (Docker only)
    Tags        map[string]string
    State       State                 // see below
    ExitCode    *int
    CreatedAt   time.Time
    EndedAt     *time.Time
    Replay      *ReplayBuffer         // in-memory circular buffer (1 MiB, not serialized)
    Handle      runtime.ProcessHandle // not serialized
    LastActivity *time.Time
    InputTokens   int
    OutputTokens  int
    CostUSD       float64
    ToolCallCount int
}
```

`session.go:14-23`

### State Machine

```
StatePending   → StatePending
StatePending   → StateRunning   (SetRunning)
StateRunning   → StateCompleted (SetCompleted, exit=0)
StateRunning   → StateFailed    (SetCompleted, exit≠0)
any            → StateOrphaned  (Manager.Recover — daemon restart)
```

**Key gap**: No `StateIdle` state. Named chats will need to add this. The session registry is **in-memory only** — it does not survive daemon restarts except via container recovery (`Manager.Recover`).

### Session Manager (`pkg/session/session.go:180`)

- Thread-safe in-memory map `map[string]*Session`
- `Add`, `Get`, `Remove`, `List`, `ShutdownAll`, `Recover`
- `SetMaxSessions(n)` for concurrency cap
- Sessions **are not persisted to disk** — only the NDJSON log files and the claude-sessions dirs survive restarts

### Log Files (`pkg/session/logfile.go`)

- Path: `{logDir}/{sessionID}.ndjson`
- `AttachSessionIO(sess, logDir)` — tees process stdout to both `Replay` buffer and the log file (`pkg/api/sessionio.go`)
- History endpoint scans log dir for `*.ndjson` files to build `SessionHistoryEntry` list

### Resume Discovery (`pkg/session/agentsessions/claude.go`)

Claude session dirs live at: `{dataDir}/claude-sessions/{agentRuntimeSessionID}/`

Structure:
```
claude-sessions/{sessionID}/
  projects/{mangled-project-path}/   ← Claude writes .jsonl here (conversation history)
  sessions/                          ← PID-based session index (*.json)
  credentials.json
  .credentials.json
```

`ReadLastClaudeSessionID(sessionDir)` — discovers the most recent Claude session ID from the sessions index, falling back to scanning `projects/**/*.jsonl` by mtime. (`agentsessions/claude.go:91`)

`ClaudeResumeArgs(dataDir, agentRuntimeSessionID)` — returns `["--resume", "--session-id", "<claudeSessionID>"]`. Returns `nil` if no prior session found (first run). (`agentsessions/claude.go:142`)

**This is how resume works**: the `agentRuntimeSessionID` passed to `ClaudeResumeArgs` must be the same ID used in the prior session — it looks up `{dataDir}/claude-sessions/{id}/` to find the JSONL.

---

## Docker Runtime & Volumes

### Volume Naming (`pkg/runtime/docker.go:201`)

```go
func dockerVolumeName(sessionID string) string {
    return "agentruntime-vol-" + sessionID
}
```

Currently volume names are derived from **session UUIDs**, not from human names. Named chats will need a different naming scheme — e.g. `agentruntime-chat-{chatName}`.

### Volume Lifecycle (`pkg/runtime/docker.go:206-235`)

- `createSessionVolume(ctx, sessionID)` — idempotent `docker volume create`, labeled `agentruntime.session_id={sessionID}`
- `RemoveSessionVolume(ctx, volumeName)` — called on DELETE with `?remove_volume=true`
- Volume is mounted at `/home/agent/.claude/projects` inside the container
- Volume persists across container restarts — this is what enables resumption

### Spawn Flow (`pkg/runtime/docker.go:123-187`)

1. `EnsureNetwork`, `EnsureProxy`
2. `prepareRun(cfg)` — calls `materializer.Materialize(req, sessionID)` to write config files, builds docker run args including mounts
3. `docker run -d ...` returns container ID
4. `waitForDockerSidecarHealth` — polls sidecar `/health` up to 15s
5. `dialSidecar` — connects to sidecar WebSocket

### Resume Wiring (handlers.go:110-124)

```go
if req.ResumeSession != "" {
    originalSession = s.sessions.Get(req.ResumeSession)
    if originalSession != nil && originalSession.VolumeName != "" {
        req.PersistSession = true  // inherit persistence
    }
}
resumeSessionID, err := s.lookupResumeSessionID(req.Agent, req.ResumeSession)
```

`lookupResumeSessionID` calls `ClaudeResumeArgs(s.dataDir, sessionID)` — **requires the original agentruntime session ID to still have its claude-sessions dir on disk**.

**Critical gap for named chats**: When a chat is idle and its session process has exited and been removed from the in-memory registry, `s.sessions.Get(req.ResumeSession)` returns nil. The chat registry must provide the volume name and last session ID independently of the in-memory session manager.

---

## Materializer (`pkg/materialize/materializer.go`)

`Materialize(req, sessionID, dataDir)` — entry point. Routes to `materializeClaude` or `materializeCodex` based on `req.Agent`.

### Claude Materialization (`materializer.go:80-217`)

1. Creates `claudeDir` at `{dataDir}/claude-sessions/{sessionID}/` via `InitClaudeSessionDir`
2. Parses `auto_discover` (discovers CLAUDE.md, settings.json, .mcp.json from `req.WorkDir`)
3. Merges discovered + explicit settings, writes `settings.json`
4. Writes `CLAUDE.md`
5. Builds MCP config from `req.Claude.McpJSON` + `req.MCPServers`, writes `.mcp.json`
6. Writes `.claude.json` (pre-trusts `/workspace`, skips onboarding)
7. Returns `Mounts` including `{claudeDir} → /home/agent/.claude (rw)`

### For named chats, the materializer needs:
- The chat's stored `SessionDir` must be the **same** directory across respawns (to preserve JSONL). The chat registry should store `SessionDir` = `{dataDir}/claude-sessions/{chatName}/` (using chat name, not UUID).
- OR: keep UUID-based session dirs but chain them — each respawn gets a new UUID dir but the Docker volume carries the JSONL. The sidecar's resume args discovery works off the volume content, not the session dir.

---

## Stall Detection (`cmd/sidecar/stall.go`)

`StallDetector` runs in the sidecar process (inside container), not in the daemon.

```go
type StallConfig struct {
    WarningTimeout time.Duration  // advisory, emits stall_warning event
    KillTimeout    time.Duration  // hard kill — calls cancelFn()
    ResultGrace    time.Duration  // post-result grace before kill
}
```

Phases (`stall.go:83-127`):
1. **Result grace** (highest priority): after `result` event seen, if process hasn't exited within `ResultGrace`, calls `cancelFn()`
2. **Hard kill**: if no events for `KillTimeout`, calls `cancelFn()`
3. **Warning**: if no events for `WarningTimeout`, emits `system/stall_warning` event

`cancelFn()` cancels the sidecar's context — kills the agent process.

### For idle timeout:
The stall detector's `KillTimeout` IS the idle timeout mechanism. Named chats can configure `StallConfig.KillTimeout = 30m` to get idle kill behavior. After the kill, the sidecar exits, the Docker container stops, and the session process exits with a non-zero code. The daemon detects this via the session handle's exit.

**Hook needed**: When a chat session's underlying session exits (completed or failed), the chat registry must transition the chat state from `running` → `idle`. This requires a callback or polling mechanism in the daemon, not the sidecar.

---

## API Layer

### Framework: Gin

### Router Setup (`pkg/api/routes.go`)

```go
func RegisterRoutes(r *gin.Engine, s *Server) {
    r.GET("/health", s.handleHealth)

    sessions := r.Group("/sessions")
    {
        sessions.POST("", s.handleCreateSession)
        sessions.GET("", s.handleListSessions)
        sessions.GET("/history", s.handleSessionHistory)
        sessions.GET("/:id", s.handleGetSession)
        sessions.GET("/:id/info", s.handleGetSessionInfo)
        sessions.GET("/:id/logs", s.handleGetLogs)
        sessions.GET("/:id/log", s.handleGetLogFile)
        sessions.DELETE("/:id", s.handleDeleteSession)
    }

    r.GET("/ws/sessions/:id", s.handleSessionWS)
    r.Static("/dashboard", "./web/dist")
}
```

**Convention**: routes registered in a single `RegisterRoutes` function. New `/chats` group goes here.

### Error Format

```json
{"error": "session not found"}
```

`c.JSON(http.StatusNotFound, gin.H{"error": "..."})` — all handlers use this pattern.

### Session Response Types (`pkg/api/schema/types.go`)

- `SessionResponse` (201): `session_id`, `task_id`, `agent`, `runtime`, `status`, `ws_url`, `log_url`
- `SessionSummary` (list): adds `created_at`, `tags`
- `SessionInfo` (detail): adds `ended_at`, `exit_code`, `session_dir`, `volume_name`, `log_file`, `uptime`, `last_activity`, token counts

### WebSocket Handler (`handlers.go:363-392`)

- Upgrades to WS via `gorilla/websocket`
- `?since=<offset>` for replay from byte offset (-1 = no replay)
- Creates `bridge.New(conn, sess.Handle, sess.Replay, logDir, sessID)` and calls `b.Run(ctx, ...)`
- **Requires an active process handle** — fails with 409 if `sess.Handle == nil`

**Gap for named chats**: Chat WS endpoint needs to handle the case where the chat is idle (no active session). It should either spawn a new session transparently or return a specific error that lets the client know to send a message first.

### SessionRequest (`pkg/api/schema/types.go:15`)

Key fields relevant to chat sessions:
```go
Agent           string          // "claude" | "codex"
Runtime         string          // "local" | "docker"
Model           string          // e.g. "claude-opus-4-5"
Prompt          string
Interactive     bool            // keep stdin open
ResumeSession   string          // session UUID to resume
PersistSession  bool            // create named Docker volume
WorkDir         string
Mounts          []Mount
Claude          *ClaudeConfig   // settings_json, claude_md, mcp_json, credentials_path, max_turns, allowed_tools
Codex           *CodexConfig
AutoDiscover    interface{}     // bool or map[string]bool
MCPServers      []MCPServer
Env             map[string]string
Container       *ContainerConfig
Timeout         string          // duration string: "5m", "30m"
```

**Chat config maps directly to SessionRequest fields** — the chat registry can store a `SessionRequest` template and populate `Prompt` + `ResumeSession` at send time.

---

## CLI Layer

### Subcommand Dispatch (`cmd/agentd/main.go:25-30`)

```go
if len(os.Args) > 1 && os.Args[1] == "dispatch" {
    os.Exit(runDispatchCommand(os.Args[2:]))
}
if len(os.Args) > 1 && os.Args[1] == "attach" {
    os.Exit(runAttachCommand(os.Args[2:]))
}
```

Pattern: string comparison on `os.Args[1]`, then dispatch to `run{Name}Command(os.Args[2:])`. Each subcommand defines its own `flag.FlagSet`.

### dispatch command (`cmd/agentd/dispatch.go`)
- `--config <yaml-path>`, `--server <url>`
- Loads YAML into `SessionRequest`, POSTs to `/sessions`, streams logs via WS
- Uses `client.New(serverURL)` from `pkg/client`

### attach command (`cmd/agentd/attach.go`)
- `<session-id>` positional arg, `--port`, `--since`, `--no-replay`
- Connects to `ws://localhost:{port}/ws/sessions/{id}?since={offset}`
- Read loop: handles `connected`, `replay`, `stdout`, `error`, `exit`, `pong` frames
- Write loop: stdin lines → `ClientFrame{type: "stdin", data: line}`
- Special: `/steer <text>` → `ClientFrame{type: "steer"}`, `/interrupt` → `ClientFrame{type: "interrupt"}`

### WebSocket Frame Types

**Server → Client** (`attach.go:170-180`):
```go
type ServerFrame struct {
    Type      string  // "connected" | "replay" | "stdout" | "error" | "exit" | "pong"
    Data      string  // base64-encoded NDJSON (for replay/stdout)
    ExitCode  *int
    Offset    int64
    SessionID string
    Mode      string
    Error     string
    Gap       bool
    Recovered bool
}
```

**Client → Server** (`attach.go:182-185`):
```go
type ClientFrame struct {
    Type string  // "stdin" | "steer" | "interrupt"
    Data string
}
```

---

## File Organization

### Where New Chat Files Should Go

Following existing patterns:

```
pkg/api/
  schema/
    types.go           — EXTEND: add ChatConfig, ChatState, ChatRecord, ChatResponse types
  routes.go            — EXTEND: add /chats route group
  handlers.go          — EXTEND: add chat handler methods OR split to chat_handlers.go
  server.go            — EXTEND: add ChatRegistry field to Server struct

pkg/chat/              — NEW PACKAGE
  registry.go          — ChatRegistry: CRUD on {dataDir}/chats/{name}.json
  types.go             — Chat struct, ChatState enum, ChatConfig

cmd/agentd/
  main.go              — EXTEND: add os.Args[1] == "chat" dispatch
  chat.go              — NEW: runChatCommand() with subcommands (create, send, list, attach, delete)
```

### Data Layout

```
{dataDir}/
  chats/
    {name}.json          — chat record (config + state + session chain + volume name)
  claude-sessions/
    {sessionUUID}/       — per-spawn session dir (JSONL lives here or in Docker volume)
  logs/
    {sessionUUID}.ndjson — event stream log per spawn
```

---

## Test Patterns

### Table-Driven Tests

Standard Go table-driven pattern, e.g. `dispatch_test.go`:
```go
tests := []struct{
    name  string
    input ...
    want  ...
}{...}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) { ... })
}
```

### Fake Runtimes

`dispatch_test.go` and `attach_test.go` use in-process test servers with fake runtimes (echo agents using `cat`, `sleep`, `sh -c` — not real Docker). Pattern: `httptest.NewServer(router)`.

### Temp Directories

`t.TempDir()` for data dirs — cleaned up automatically.

### Error Assertions

Exact string match: `if err == nil || !strings.Contains(err.Error(), "expected")`.

### No test framework — stdlib `testing` only.

---

## Integration Points

### Files to Create

| File | Purpose |
|------|---------|
| `pkg/chat/types.go` | `ChatState`, `ChatRecord`, `ChatConfig` structs |
| `pkg/chat/registry.go` | JSON file-based registry for chat records |
| `cmd/agentd/chat.go` | CLI subcommand: `agentd chat <create|send|list|delete>` |

### Files to Modify

| File | Change |
|------|--------|
| `pkg/api/schema/types.go` | Add `ChatRequest`, `ChatSendRequest`, `ChatResponse`, `ChatSummary` |
| `pkg/api/routes.go` | Add `/chats` route group |
| `pkg/api/handlers.go` | Add chat handler methods (or split to `chat_handlers.go`) |
| `pkg/api/server.go` | Add `chatRegistry *chat.Registry` field, wire in `NewServer` |
| `cmd/agentd/main.go` | Add `os.Args[1] == "chat"` dispatch block |

### What the Feature Needs from Each Component

**Session Manager**: No changes needed. Named chats spawn regular sessions and track the resulting UUID in the chat record.

**Docker Runtime**: No changes needed. Volume name is passed in `SpawnConfig.VolumeName`. Just need to use `agentruntime-chat-{name}` instead of `agentruntime-vol-{uuid}`.

**Materializer**: No changes needed. The chat's stored `SessionRequest` template is passed as-is; materializer handles it.

**Stall Detector**: No changes needed. `KillTimeout` in `StallConfig` is the idle timeout. Chat feature configures this at dispatch time via `req.Timeout` or a sidecar env var.

**ClaudeResumeArgs**: This takes an `agentRuntimeSessionID` to look up the session dir. For named chats using Docker volumes, the JSONL lives in the volume, not the session dir. **This needs investigation** — either:
  a. The chat registry reuses the same agentruntime session ID across respawns (so the session dir accumulates JSONL across spawns), OR
  b. The sidecar discovers the Claude session ID directly from the Docker volume content

**AttachSessionIO**: Called in handler after spawn — no changes needed. Chat's active session is a regular session in the manager.

---

## Key Design Questions for Spec

1. **Volume naming**: `agentruntime-chat-{name}` vs `agentruntime-vol-{uuid}` (with the UUID stored in chat record). The former is simpler; the latter avoids name collision if chats are deleted and recreated.

2. **Resume session ID lookup**: `ClaudeResumeArgs` requires the agentruntime session ID to look in `claude-sessions/{id}/`. For chats, options:
   - Store the last agentruntime session ID in the chat record and pass it as `ResumeSession`
   - Use a stable chat-named session dir (`claude-sessions/chat-{name}/`) reused across spawns
   - Let the Docker volume carry the JSONL and have the sidecar discover it from the volume (requires sidecar change)

3. **Idle detection hook**: Stall detector kills the agent process. The daemon detects process exit via the session handle's exit channel. Where does the "chat state → idle" transition happen? Options:
   - Background goroutine polling session states for chat-tagged sessions
   - Callback registered when session completes (would require session Manager change)
   - On next `POST /chats/:name/messages` — lazy state transition

4. **Concurrent message handling**: If two messages arrive while the chat is idle, both will try to spawn a new session. Options:
   - Mutex per chat in the registry (reject second until first spawns)
   - Queue: buffer messages and drain when session is running
   - Atomic state with CAS: only the winner spawns, others wait

5. **Chat WS (`GET /ws/chats/:name`)**: When chat is idle (no active session), should this:
   - Block and wait for next message to spawn a session?
   - Return 409 with `{"error": "chat is idle"}`?
   - Auto-spawn an interactive session?

6. **Message history across respawns**: The replay buffer is per-session (in-memory, 1 MiB). Chat history across multiple spawns requires either scanning all log files for sessions tagged to this chat, or a dedicated chat log file that aggregates across sessions.

7. **Chat name validation**: Should follow `sanitizeSessionID` pattern (alphanumeric + hyphen + underscore) but using a human-facing name. Max length?

8. **PATCH /chats/:name/config**: Which fields can be updated after creation? Model/effort/env are safe; changing agent type or work_dir mid-chat has implications for resume.

9. **DELETE /chats/:name**: Should `?remove_volume=true` also be supported? Should deleting a running chat kill the active session?

10. **State machine on daemon restart**: Chat records are on disk (survive restarts). Sessions are in-memory (don't survive). On startup, chats whose last session UUID is not in the recovered session list should transition to `idle`.
