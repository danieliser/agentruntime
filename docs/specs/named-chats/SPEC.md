# Plan: Named Persistent Chat Sessions

**Version:** 1.0
**Date:** 2026-03-19
**Status:** Draft
**Branch:** `feat/named-chats`

---

## Meta

### Problem Statement

Agent sessions in agentruntime today are fire-and-forget: a session is created, the agent runs to completion, and the session exits. There is no concept of a named, long-lived conversation that an operator can return to across multiple prompts. When PAOP's `persist chat web-ui` launches a Claude session, it must manage all lifetime state itself — tracking the agentruntime session ID, deciding when to resume vs. respawn, and handling Docker volume continuity. This logic belongs in agentruntime, not every caller.

Three structural gaps cause this:

1. **No named session registry.** Sessions are identified by UUID only. There is no stable human-readable handle that survives session rotation.

2. **No spawn-on-demand.** Callers must track session liveness and manually issue a new `POST /sessions` with `resume_session` when the agent has exited. There is no single endpoint that "send me a message, spawn if needed."

3. **No idle lifecycle.** A running agent consumes resources even when no new messages arrive. There is no built-in mechanism to kill an idle agent process while preserving conversation state for the next message.

### Proposed Solution

Add a first-class **chat** abstraction to agentruntime: a named, persistent, resumable conversation backed by a stable Docker volume. A chat has a stored config (agent, model, runtime, MCP servers, etc.) that is specified once at creation and reused on every respawn. An idle timeout (default 30 min) kills the agent process when the chat goes quiet, preserving the volume for the next message. When a new message arrives for an idle chat, the daemon respawns the agent with `--resume` wired to the preserved volume and the last Claude session ID — giving the agent full context continuity.

The feature is additive. Existing `/sessions` endpoints and behavior are unchanged.

### Key Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Registry storage | `{dataDir}/chats/{name}.json` per chat | File-per-record survives daemon restart without migration. Simple atomic writes. Matches agentruntime's existing per-session log file convention. |
| Volume naming | `agentruntime-chat-{name}` | Deterministic — stable across session ID rotation. Human-readable. Docker volume names allow alphanumeric + hyphens/underscores. |
| Concurrent message handling | Reject-with-429 while processing; queue one pending | Prevents conversation corruption. Single pending slot avoids unbounded queue growth. 429 body includes estimated wait. |
| Idle timeout mechanism | Daemon-level watcher polling `Session.LastActivity` | Stall detection is sidecar-side and per-process. Chat idle timeout is a daemon-level concern — same layer that manages session lifecycle. Configurable per chat. |
| Config mutability | PATCH allowed only when `state == idle` | Mutating MCP servers or work_dir mid-conversation would create inconsistent context. Idle is the safe window. |
| State machine | `created → running → idle → running → deleted` | Simple four-state machine. `created` is a transient state (first message transitions immediately to `running`). |
| Session chain | Array of session IDs in chat record | Full history preserved. Last entry is `CurrentSession`. Useful for debugging, log retrieval, and cost accounting. |
| Resume Claude session ID | `Session.Tags["claude_session_id"]` from last chain entry | Sidecar already emits this tag (per session-volumes spec). No Docker volume inspection required. |
| PAOP integration | New agentd chat API methods in `AgentRuntimeClient` + switch `persist chat` to call agentd | Removes session management duplication from PAOP; agentruntime owns the lifecycle. |
| Local runtime support | Local runtime allowed; volume not created | Named volumes are Docker-only. Local runtime uses host-side session dir for continuity. `--resume` still works. |
| Message history | Log-file-based reader per session in chain | No separate message DB. Session NDJSON logs contain all events. Reader concatenates across chain with offsets. |

### Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│ agentd daemon                                                           │
│                                                                         │
│  ChatRegistry                   ChatManager                            │
│  ├── {dataDir}/chats/*.json     ├── Spawn(name, prompt) → SessionID   │
│  ├── Load/Save/List/Delete      ├── Respawn(name, prompt)              │
│  └── Locking (file lock)        ├── SendMessage(name, msg)             │
│                                  ├── Idle(name)                         │
│  ChatIdleWatcher                └── Delete(name)                       │
│  └── Tick every 30s                                                     │
│      → check LastActivity on chat-backed sessions                      │
│      → Kill() + transition to idle when expired                        │
│                                                                         │
│  API (pkg/api/chat_handlers.go)                                        │
│  POST   /chats                    → ChatManager.CreateChat()           │
│  GET    /chats                    → ChatRegistry.List()                │
│  GET    /chats/:name              → ChatRegistry.Load(name)            │
│  POST   /chats/:name/messages     → ChatManager.SendMessage()          │
│  GET    /chats/:name/messages     → LogReader.Read(chain)              │
│  PATCH  /chats/:name/config       → ChatRegistry.UpdateConfig()        │
│  GET    /ws/chats/:name           → WS proxy to current session        │
│  DELETE /chats/:name              → ChatManager.Delete()               │
└─────────────────────────────────────────────────────────────────────────┘
         │ Session.Tags["chat_name"] links sessions back to chat
         ▼
┌─────────────────────────┐
│ session.Manager (existing) │
│ SessionRequest + Runtimes  │
└─────────────────────────┘
```

**Volume continuity across respawns:**

```
POST /chats/web-ui/messages {message: "hello"}
  │
  ├─ chat.state == "idle" (or "created")
  │    └── ChatManager.Spawn(name, message)
  │         └── SessionRequest{
  │               ResumeSession:  lastClaudeSessionID,   ← from Session.Tags
  │               PersistSession: true,
  │               Mounts:         [{Host: "agentruntime-chat-web-ui",
  │                                Container: "/home/agent/.claude/projects",
  │                                Type: "volume", Mode: "rw"}],
  │               Tags:           {"chat_name": "web-ui"},
  │             }
  │         └── session created → appended to chat.SessionChain
  │         └── chat.State = "running"
  │
  └─ chat.state == "running"
       └── WS stdin inject OR queue as pending
```

---

## Phase 1: Types and Registry

**Goal:** Define the chat data model and file-based registry. No daemon integration yet.

### Task 1.1: ChatRecord, ChatConfig, ChatState types

**File(s):** `pkg/chat/types.go` (new file)

**What:**
Define all types for the chat subsystem:

```go
package chat

import "time"

type ChatState string

const (
    ChatStateCreated ChatState = "created"
    ChatStateRunning ChatState = "running"
    ChatStateIdle    ChatState = "idle"
    ChatStateDeleted ChatState = "deleted"
)

// ChatRecord is the persisted representation of a named chat.
// Stored as JSON in {dataDir}/chats/{name}.json.
type ChatRecord struct {
    Name           string            `json:"name"`
    Config         ChatConfig        `json:"config"`
    State          ChatState         `json:"state"`
    VolumeName     string            `json:"volume_name,omitempty"`     // "agentruntime-chat-{name}", empty for local runtime
    CurrentSession string            `json:"current_session,omitempty"` // session ID of the live/last session
    SessionChain   []string          `json:"session_chain"`             // all session IDs, oldest first
    PendingMessage string            `json:"pending_message,omitempty"` // queued message (max 1)
    CreatedAt      time.Time         `json:"created_at"`
    UpdatedAt      time.Time         `json:"updated_at"`
    LastActiveAt   *time.Time        `json:"last_active_at,omitempty"`
}

// ChatConfig is the stored config applied to every spawned session.
// Specified at creation and reused on respawn. PATCH-able only when idle.
type ChatConfig struct {
    Agent        string            `json:"agent"`
    Runtime      string            `json:"runtime,omitempty"`        // "local" | "docker" (default: daemon default)
    Model        string            `json:"model,omitempty"`
    Effort       string            `json:"effort,omitempty"`         // informational; passed as tag
    MCPServers   []MCPServer       `json:"mcp_servers,omitempty"`
    AutoDiscover interface{}       `json:"auto_discover,omitempty"`
    WorkDir      string            `json:"work_dir,omitempty"`
    Env          map[string]string `json:"env,omitempty"`
    IdleTimeout  string            `json:"idle_timeout,omitempty"`   // duration string, default "30m"
    MaxTurns     int               `json:"max_turns,omitempty"`
}
```

`MCPServer` re-uses `schema.MCPServer` from `pkg/api/schema/types.go` — import and alias, do not duplicate.

**DoD:**
- `pkg/chat/types.go` compiles with no warnings.
- `ChatState` has `String()` and `IsTerminal()` methods (`deleted` is terminal).
- `ChatConfig.EffectiveIdleTimeout()` parses `IdleTimeout` and returns `time.Duration`, defaulting to 30 minutes.
- `ChatRecord.LastSessionID()` returns the last entry in `SessionChain` or `""`.

---

### Task 1.2: ChatRegistry — file-based CRUD

**File(s):** `pkg/chat/registry.go` (new file)

**What:**
File-based registry backed by `{dataDir}/chats/`. Each chat is a JSON file named `{name}.json`. File locking prevents concurrent writes.

```go
type Registry struct {
    dir string // {dataDir}/chats
}

func NewRegistry(dataDir string) (*Registry, error)

// Save writes the record atomically (write to .tmp, rename).
func (r *Registry) Save(rec *ChatRecord) error

// Load reads a chat record by name. Returns ErrNotFound if absent.
func (r *Registry) Load(name string) (*ChatRecord, error)

// List returns all chat records sorted by CreatedAt ascending.
func (r *Registry) List() ([]*ChatRecord, error)

// Delete removes the chat record file. Does not touch the volume.
func (r *Registry) Delete(name string) error

// Exists reports whether a chat with the given name exists.
func (r *Registry) Exists(name string) bool
```

**Implementation notes:**
- `Save` uses atomic write: `os.WriteFile` to `{name}.tmp`, then `os.Rename` to `{name}.json`.
- No global mutex — callers hold their own concurrency invariant. Concurrent saves to the same name are rare; atomic rename prevents corruption.
- `ErrNotFound` is a package-level sentinel: `var ErrNotFound = errors.New("chat not found")`.
- Chat name validation: `[a-z0-9][a-z0-9-_]*`, max 64 chars. A helper `ValidateName(name) error` enforces this.

**DoD:**
- `Registry` compiles with no external dependencies beyond `encoding/json`, `os`, `path/filepath`, `sort`.
- Round-trip test: `Save` + `Load` returns identical record.
- `List` returns records sorted by `CreatedAt` ascending.
- `ValidateName` rejects names with uppercase, spaces, leading hyphens, or >64 chars.
- `ErrNotFound` is returned (not a generic error) when file is absent.

---

### Task 1.3: Types unit tests

**File(s):** `pkg/chat/registry_test.go` (new file), `pkg/chat/types_test.go` (new file)

**What:**
Full unit coverage for types and registry:

| Test | What it verifies |
|------|-----------------|
| `TestChatState_String` | All four states have correct string values |
| `TestChatState_IsTerminal` | Only `deleted` is terminal |
| `TestChatConfig_EffectiveIdleTimeout_Default` | Empty string → 30 minutes |
| `TestChatConfig_EffectiveIdleTimeout_Custom` | "1h" → 1 hour |
| `TestChatConfig_EffectiveIdleTimeout_Invalid` | Garbage string → 30 minute default |
| `TestChatRecord_LastSessionID_Empty` | Empty chain → "" |
| `TestChatRecord_LastSessionID_NonEmpty` | Returns last entry |
| `TestRegistry_SaveLoad_RoundTrip` | Save then Load returns identical struct |
| `TestRegistry_Load_NotFound` | Returns `ErrNotFound` |
| `TestRegistry_List_Sorted` | Three records with different CreatedAt → ascending order |
| `TestRegistry_Delete` | File removed; subsequent Load returns ErrNotFound |
| `TestRegistry_Exists` | True for present, false for absent |
| `TestValidateName_Valid` | "web-ui", "chat1", "my_chat" pass |
| `TestValidateName_Invalid` | "", "Web-UI", "a b", "-bad", 65-char string all fail |
| `TestRegistry_AtomicWrite` | After failed mid-write simulation, no corruption |

**DoD:** All tests pass with `go test ./pkg/chat/...`.

---

## Phase 2: Chat Manager

**Goal:** Business logic for spawning, respawning, sending messages, and transitioning chat state. Wires into the existing session manager and runtimes.

### Task 2.1: ChatManager struct and constructor

**File(s):** `pkg/chat/manager.go` (new file)

**What:**

```go
type Manager struct {
    registry *Registry
    sessions *session.Manager    // existing session manager
    runtimes map[string]runtime.Runtime // keyed by name
    defaultRuntime string
    mu       sync.Mutex          // protects per-chat operation serialization
}

func NewManager(
    registry *Registry,
    sessions *session.Manager,
    runtimes map[string]runtime.Runtime,
    defaultRuntime string,
) *Manager
```

The `mu` mutex serializes operations per chat using a per-name lock map (`map[string]*sync.Mutex`) to avoid global contention. Operations on different chats proceed concurrently.

```go
// chatLock returns (or lazily creates) the per-chat mutex.
func (m *Manager) chatLock(name string) *sync.Mutex
```

**DoD:**
- `NewManager` compiles and returns a non-nil `*Manager`.
- `chatLock` returns a stable mutex for the same name and different mutexes for different names.

---

### Task 2.2: CreateChat, GetChat, ListChats, DeleteChat

**File(s):** `pkg/chat/manager.go`

**What:**

```go
// CreateChat creates a new named chat with the given config.
// Returns ErrAlreadyExists if a chat with this name already exists.
func (m *Manager) CreateChat(name string, cfg ChatConfig) (*ChatRecord, error)

// GetChat returns the current ChatRecord for the named chat.
func (m *Manager) GetChat(name string) (*ChatRecord, error)

// ListChats returns all chat records.
func (m *Manager) ListChats() ([]*ChatRecord, error)

// DeleteChat transitions the chat to deleted state.
// If running, kills the current session first (with 5s grace).
// Does not remove the Docker volume — callers do that explicitly.
func (m *Manager) DeleteChat(name string, removeVolume bool) error
```

`CreateChat` logic:
1. `ValidateName(name)`.
2. Check `registry.Exists(name)` → return `ErrAlreadyExists` if true.
3. Determine volume name: if `cfg.Runtime == "docker"` (or empty, will default to docker), set `VolumeName = "agentruntime-chat-{name}"`. Create the Docker volume via `docker volume create` with label `agentruntime.chat_name={name}`.
4. Build `ChatRecord{State: ChatStateCreated, SessionChain: []string{}, Config: cfg, VolumeName: volumeName, CreatedAt: now, UpdatedAt: now}`.
5. `registry.Save(rec)`.
6. Return record.

`DeleteChat` logic:
1. Load record; return `ErrNotFound` if absent.
2. If `state == running`, call `killCurrentSession(name, 5*time.Second)`.
3. If `removeVolume && rec.VolumeName != ""`, run `docker volume rm {rec.VolumeName}`.
4. `registry.Delete(name)`.

**DoD:**
- `CreateChat` creates volume for docker runtime, skips volume creation for `runtime: "local"`.
- Duplicate create returns `ErrAlreadyExists`.
- `DeleteChat` on a running chat kills the session before removing the record.
- `DeleteChat` with `removeVolume: true` calls `docker volume rm`.

---

### Task 2.3: SendMessage — spawn-on-demand and stdin injection

**File(s):** `pkg/chat/manager.go`

**What:**
Core send-message logic that powers `POST /chats/:name/messages`.

```go
type SendResult struct {
    SessionID  string    // session handling this message
    Queued     bool      // true if chat was running and message was queued
    Spawned    bool      // true if a new session was spawned
    CreatedAt  time.Time
}

// SendMessage sends a message to the named chat.
//   - If state is created/idle: spawns a new session with the message as the initial prompt.
//   - If state is running + no pending: injects message via WS stdin to running session.
//   - If state is running + pending slot taken: returns ErrChatBusy.
func (m *Manager) SendMessage(name, message string) (*SendResult, error)
```

`SendMessage` logic under per-chat lock:

```
1. Load record.
2. state == deleted → return ErrNotFound
3. state == created || state == idle
     → spawnSession(name, message)          ← new session
     → record.State = running
     → record.CurrentSession = newSessionID
     → record.SessionChain = append(chain, newSessionID)
     → Save
     → return SendResult{SessionID: newID, Spawned: true}
4. state == running
     a. pending == "" → inject via WS stdin to current session
                      → record.LastActiveAt = now
                      → Save
                      → return SendResult{SessionID: currentID}
     b. pending != "" → return ErrChatBusy (429)
```

`spawnSession(name, message)` builds a `SessionRequest` from the chat config:
- `Prompt = message`
- `Agent = cfg.Agent`
- `Runtime = cfg.Runtime` (empty → daemon default)
- `Model = cfg.Model`
- `Tags = {"chat_name": name, "effort": cfg.Effort}`
- `MCPServers = cfg.MCPServers`
- `AutoDiscover = cfg.AutoDiscover`
- `WorkDir = cfg.WorkDir`
- `Env = cfg.Env`
- `MaxTurns = cfg.MaxTurns` (via `Claude.MaxTurns`)
- `PersistSession = true` (when docker runtime)
- `Interactive = true` (keeps stdin open for subsequent messages)
- If `rec.LastSessionID() != ""` and `rec.VolumeName != ""`:
  - `ResumeSession = lastClaudeSessionID` (from `Session.Tags["claude_session_id"]`)
  - Mounts: `[{Host: rec.VolumeName, Container: "/home/agent/.claude/projects", Type: "volume", Mode: "rw"}]`

**DoD:**
- `SendMessage` on an idle chat spawns a new session and returns `Spawned: true`.
- `SendMessage` on a running chat with empty pending injects via WS stdin.
- `SendMessage` on a running chat with a pending message returns `ErrChatBusy`.
- `spawnSession` correctly sets `ResumeSession` from the last session's `claude_session_id` tag when the session chain is non-empty.
- `spawnSession` skips `ResumeSession` and volume mount for local runtime.

---

### Task 2.4: Session exit watcher and idle transition

**File(s):** `pkg/chat/manager.go`

**What:**
When a chat-backed session exits, the chat must transition to `idle` and any pending message must trigger a respawn.

```go
// WatchSession registers an exit watcher for a chat-backed session.
// Called by the API handler after spawn, or by the idle watcher after respawn.
// On session exit: transitions chat to idle, then respawns if pending exists.
func (m *Manager) WatchSession(name, sessionID string)
```

Implementation: launch a goroutine that polls `session.Manager.Get(sessionID)` status every 2 seconds. When status is terminal (`completed`, `failed`, `exited`, `killed`, `dead`):
1. Acquire per-chat lock.
2. Load record.
3. If `record.CurrentSession != sessionID`, return (stale watcher — a newer session has taken over).
4. Capture `claude_session_id` from session tags and store in record's last-session metadata (see §Task 2.5).
5. If `record.PendingMessage != ""`:
   - Clear `PendingMessage`, save record.
   - `spawnSession(name, pendingMessage)` with the pending message.
   - `WatchSession(name, newSessionID)`.
6. Else:
   - `record.State = idle`, `record.CurrentSession = ""`.
   - Save record.

**DoD:**
- After session exits, chat transitions to `idle`.
- If pending message exists when session exits, it is consumed and a new session spawns.
- Stale watchers (from a replaced session) do not corrupt state.

---

### Task 2.5: Claude session ID tracking

**File(s):** `pkg/chat/manager.go`, `pkg/chat/types.go`

**What:**
`--resume` requires the Claude session ID (the `.jsonl` filename, not the agentruntime session ID). This is emitted by the sidecar as `Session.Tags["claude_session_id"]` (per the session-volumes spec).

Add to `ChatRecord`:
```go
// ClaudeSessionIDs maps agentruntime session ID → Claude session ID.
// Populated when a session exits, used when spawning the next session.
ClaudeSessionIDs map[string]string `json:"claude_session_ids,omitempty"`
```

In `WatchSession`, after session exit, before transitioning to idle:
```go
if claudeID, ok := sess.Tags["claude_session_id"]; ok && claudeID != "" {
    if rec.ClaudeSessionIDs == nil {
        rec.ClaudeSessionIDs = make(map[string]string)
    }
    rec.ClaudeSessionIDs[sessionID] = claudeID
}
```

In `spawnSession`, resolve `resumeClaudeSessionID`:
```go
// Find the claude_session_id for the most recent session in the chain.
if lastID := rec.LastSessionID(); lastID != "" {
    if claudeID, ok := rec.ClaudeSessionIDs[lastID]; ok {
        resumeClaudeSessionID = claudeID
    }
}
```

**DoD:**
- After a session completes, `rec.ClaudeSessionIDs[sessionID]` is populated if the sidecar reported the tag.
- `spawnSession` uses the stored Claude session ID for `ResumeSession`.
- Works correctly when `ClaudeSessionIDs` is nil (no prior sessions).

---

### Task 2.6: Manager unit tests

**File(s):** `pkg/chat/manager_test.go` (new file)

**What:**

| Test | What it verifies |
|------|-----------------|
| `TestCreateChat_Docker_CreatesVolume` | Volume create call issued with correct label |
| `TestCreateChat_Local_NoVolume` | No volume create for local runtime |
| `TestCreateChat_DuplicateName` | Returns `ErrAlreadyExists` |
| `TestSendMessage_IdleChat_SpawnsSession` | Idle → running, session appended to chain |
| `TestSendMessage_RunningChat_InjectsStdin` | Running + no pending → WS inject called |
| `TestSendMessage_RunningChat_PendingExists` | Returns `ErrChatBusy` |
| `TestSendMessage_ResumeSetFromClaudeSessionID` | `ResumeSession` populated from `ClaudeSessionIDs` |
| `TestSendMessage_FirstSpawn_NoResume` | No `ResumeSession` on first spawn |
| `TestWatchSession_ExitTransitionsToIdle` | Session exit → chat state becomes idle |
| `TestWatchSession_PendingConsumedOnExit` | Pending message triggers respawn on exit |
| `TestWatchSession_StaleWatcherIgnored` | Stale watcher does not overwrite newer session |
| `TestDeleteChat_KillsRunningSession` | Kill called before delete when running |
| `TestDeleteChat_RemoveVolume` | `docker volume rm` called when `removeVolume: true` |
| `TestClaudeSessionIDTracking_RoundTrip` | Tag captured, used in next spawn |

Use fakes/stubs for `session.Manager` and Docker runtime calls.

**DoD:** All tests pass with `go test ./pkg/chat/...`.

---

## Phase 3: Idle Watcher

**Goal:** Daemon-level goroutine that monitors chat-backed sessions for inactivity and transitions them to idle when the configured timeout expires.

### Task 3.1: ChatIdleWatcher

**File(s):** `pkg/chat/idle_watcher.go` (new file)

**What:**

```go
type IdleWatcher struct {
    registry *Registry
    sessions *session.Manager
    manager  *Manager
    interval time.Duration // poll interval, default 30s
    done     chan struct{}
}

func NewIdleWatcher(
    registry *Registry,
    sessions *session.Manager,
    manager *Manager,
) *IdleWatcher

func (w *IdleWatcher) Start(ctx context.Context)
func (w *IdleWatcher) Stop()
```

`Start` launches a goroutine that ticks every `w.interval`:

```
tick:
  chats = registry.List()
  for each chat where state == "running":
    sess = sessions.Get(chat.CurrentSession)
    if sess == nil or sess.Status is terminal:
        // session already dead, manager.WatchSession will handle it
        continue
    idleTimeout = chat.Config.EffectiveIdleTimeout()
    if sess.LastActivity != nil && time.Since(*sess.LastActivity) > idleTimeout:
        log.Printf("chat %q idle for %s, killing session %s", chat.Name, idleTimeout, chat.CurrentSession)
        sess.Kill()
        // WatchSession goroutine handles the state transition
```

The watcher does **not** transition state directly — it kills the process and relies on `WatchSession` to handle the resulting exit event. This avoids race conditions between the watcher and the exit watcher.

**DoD:**
- Watcher ticks every 30s by default (configurable via `interval` field).
- Kills session when `LastActivity` is older than `IdleTimeout`.
- Does not double-kill (checks terminal status before acting).
- Stops cleanly on context cancellation.

---

### Task 3.2: Idle watcher tests

**File(s):** `pkg/chat/idle_watcher_test.go` (new file)

**What:**

| Test | What it verifies |
|------|-----------------|
| `TestIdleWatcher_KillsIdleSession` | Session with LastActivity > timeout is killed |
| `TestIdleWatcher_SkipsActiveSession` | Session with recent activity is not killed |
| `TestIdleWatcher_SkipsTerminalSession` | Already-exited session not double-killed |
| `TestIdleWatcher_SkipsIdleChats` | Idle-state chats are not inspected |
| `TestIdleWatcher_CustomTimeout` | Per-chat IdleTimeout respected |
| `TestIdleWatcher_Stop` | Context cancellation stops the loop |

Use fake session manager with controllable `LastActivity` and `Status`.

**DoD:** All tests pass.

---

## Phase 4: API Layer

**Goal:** REST handlers and WebSocket proxy for named chats. Follows existing `pkg/api/handlers.go` patterns.

### Task 4.1: Request/response types

**File(s):** `pkg/api/schema/types.go`

**What:**
Add chat API types to the existing schema package:

```go
// CreateChatRequest is the body for POST /chats.
type CreateChatRequest struct {
    Name   string     `json:"name"`   // required
    Config ChatConfig `json:"config"` // required
}

// ChatConfig mirrors chat.ChatConfig for API consumers.
type ChatConfig struct {
    Agent        string            `json:"agent"`
    Runtime      string            `json:"runtime,omitempty"`
    Model        string            `json:"model,omitempty"`
    Effort       string            `json:"effort,omitempty"`
    MCPServers   []MCPServer       `json:"mcp_servers,omitempty"`
    AutoDiscover interface{}       `json:"auto_discover,omitempty"`
    WorkDir      string            `json:"work_dir,omitempty"`
    Env          map[string]string `json:"env,omitempty"`
    IdleTimeout  string            `json:"idle_timeout,omitempty"`
    MaxTurns     int               `json:"max_turns,omitempty"`
}

// ChatResponse is returned by GET /chats/:name and POST /chats.
type ChatResponse struct {
    Name           string            `json:"name"`
    Config         ChatConfig        `json:"config"`
    State          string            `json:"state"`
    VolumeName     string            `json:"volume_name,omitempty"`
    CurrentSession string            `json:"current_session,omitempty"`
    SessionChain   []string          `json:"session_chain"`
    CreatedAt      time.Time         `json:"created_at"`
    UpdatedAt      time.Time         `json:"updated_at"`
    LastActiveAt   *time.Time        `json:"last_active_at,omitempty"`
    WSURL          string            `json:"ws_url,omitempty"`   // present when running
}

// ChatSummary is returned by GET /chats.
type ChatSummary struct {
    Name         string     `json:"name"`
    State        string     `json:"state"`
    Agent        string     `json:"agent"`
    Runtime      string     `json:"runtime,omitempty"`
    SessionCount int        `json:"session_count"`
    CreatedAt    time.Time  `json:"created_at"`
    LastActiveAt *time.Time `json:"last_active_at,omitempty"`
}

// SendMessageRequest is the body for POST /chats/:name/messages.
type SendMessageRequest struct {
    Message string `json:"message"` // required
}

// SendMessageResponse is returned by POST /chats/:name/messages.
type SendMessageResponse struct {
    SessionID string `json:"session_id"`
    State     string `json:"state"`
    Queued    bool   `json:"queued,omitempty"`
    Spawned   bool   `json:"spawned,omitempty"`
    WSURL     string `json:"ws_url"`
}

// ChatMessageEntry is one entry in GET /chats/:name/messages.
type ChatMessageEntry struct {
    SessionID string          `json:"session_id"`
    Type      string          `json:"type"`       // "agent_message" | "tool_use" | "tool_result" | "result" | "system"
    Data      json.RawMessage `json:"data"`
    Offset    int64           `json:"offset"`
    Timestamp time.Time       `json:"timestamp"`
}

// ChatMessagesResponse is returned by GET /chats/:name/messages.
type ChatMessagesResponse struct {
    Messages []ChatMessageEntry `json:"messages"`
    Total    int                `json:"total"`
    HasMore  bool               `json:"has_more"`
    Before   int64              `json:"before,omitempty"` // cursor for pagination
}

// UpdateChatConfigRequest is the body for PATCH /chats/:name/config.
type UpdateChatConfigRequest struct {
    Config ChatConfig `json:"config"` // full replacement (not partial merge)
}
```

**DoD:** Types compile. No duplicate fields with existing schema types.

---

### Task 4.2: Chat API handlers

**File(s):** `pkg/api/chat_handlers.go` (new file)

**What:**
Implement all chat handlers on `*Server`:

**`handleCreateChat`** — `POST /chats`
1. Bind `CreateChatRequest`. Validate name and config (agent required).
2. `manager.CreateChat(req.Name, cfg)`.
3. Return `201 Created` with `ChatResponse`.
4. Return `409 Conflict` if `ErrAlreadyExists`.

**`handleListChats`** — `GET /chats`
1. `manager.ListChats()`.
2. Map records to `[]ChatSummary`.
3. Return `200 OK`.

**`handleGetChat`** — `GET /chats/:name`
1. `manager.GetChat(name)`.
2. Build `ChatResponse`. If `state == running`, populate `WSURL = ws://localhost:{port}/ws/chats/{name}`.
3. Return `200 OK` or `404 Not Found`.

**`handleSendMessage`** — `POST /chats/:name/messages`
1. Bind `SendMessageRequest`. Require non-empty message.
2. `manager.SendMessage(name, req.Message)`.
3. On `ErrChatBusy`: return `429 Too Many Requests` with body `{"error": "chat is busy", "retry_after_ms": 5000}`.
4. On success: return `202 Accepted` with `SendMessageResponse`.

**`handleGetChatMessages`** — `GET /chats/:name/messages`
1. Parse query params: `limit` (default 100, max 500), `before` (offset cursor).
2. `manager.GetChat(name)` → get `SessionChain`.
3. Use `LogReader` to read NDJSON events from session logs, concatenating across chain with offset accounting.
4. Filter to message-bearing event types: `agent_message`, `tool_use`, `tool_result`, `result`, `system`.
5. Apply `limit` and `before` cursor.
6. Return `200 OK` with `ChatMessagesResponse`.

**`handleUpdateChatConfig`** — `PATCH /chats/:name/config`
1. Load record. If `state != idle` and `state != created`: return `409 Conflict` with `{"error": "config can only be updated when chat is idle"}`.
2. Bind `UpdateChatConfigRequest`. Validate agent present.
3. If runtime changed to/from docker, update volume name accordingly (create new volume if docker; leave old volume if demoting — just update VolumeName field to empty).
4. Save updated config + `UpdatedAt = now`.
5. Return `200 OK` with updated `ChatResponse`.

**`handleDeleteChat`** — `DELETE /chats/:name`
1. Parse `?remove_volume=true` query param.
2. `manager.DeleteChat(name, removeVolume)`.
3. Return `204 No Content` or `404 Not Found`.

**`handleChatWS`** — `GET /ws/chats/:name`
1. `manager.GetChat(name)` → require `state == running`.
2. Get current session ID. Look up session in `sessions.Manager`.
3. Proxy the WS connection: upgrade, then forward frames bidirectionally between the client and `/ws/sessions/{currentSessionID}`. Use existing `wsUpgrader`.
4. If chat is not running: return `409 Conflict` (not yet running or already idle — client should poll `GET /chats/:name` and retry).

**DoD:**
- All handlers follow existing gin patterns (no panics, consistent error JSON).
- `handleSendMessage` returns `429` with `retry_after_ms` on busy.
- `handleChatWS` returns `409` when not running.
- `handleUpdateChatConfig` returns `409` when running.

---

### Task 4.3: Route registration

**File(s):** `pkg/api/routes.go`

**What:**
Add chat routes to `RegisterRoutes`:

```go
chats := r.Group("/chats")
{
    chats.POST("", s.handleCreateChat)
    chats.GET("", s.handleListChats)
    chats.GET("/:name", s.handleGetChat)
    chats.POST("/:name/messages", s.handleSendMessage)
    chats.GET("/:name/messages", s.handleGetChatMessages)
    chats.PATCH("/:name/config", s.handleUpdateChatConfig)
    chats.DELETE("/:name", s.handleDeleteChat)
}

r.GET("/ws/chats/:name", s.handleChatWS)
```

Wire `ChatManager` and `ChatRegistry` into `Server` struct:

```go
// pkg/api/server.go — add to Server struct
type Server struct {
    // ... existing fields ...
    chatRegistry *chat.Registry
    chatManager  *chat.Manager
    chatWatcher  *chat.IdleWatcher
}
```

Update `NewServer` to accept and store these.

**DoD:**
- Routes registered correctly (verify with `curl /chats` → not 404).
- `Server` holds references to chat subsystem components.

---

### Task 4.4: API handler tests

**File(s):** `pkg/api/api_test.go` (extend existing), new `pkg/api/chat_api_test.go`

**What:**

| Test | What it verifies |
|------|-----------------|
| `TestCreateChat_201` | Valid request → 201, body has name/state/volume |
| `TestCreateChat_409_Duplicate` | Duplicate name → 409 |
| `TestCreateChat_400_MissingAgent` | No agent → 400 |
| `TestCreateChat_400_InvalidName` | Name with uppercase → 400 |
| `TestListChats_Empty` | Empty registry → 200, empty array |
| `TestListChats_Multiple` | Returns all chats sorted by CreatedAt |
| `TestGetChat_200` | Present chat → 200 with full ChatResponse |
| `TestGetChat_404` | Missing name → 404 |
| `TestGetChat_Running_HasWSURL` | Running chat → ws_url populated |
| `TestSendMessage_202_Spawned` | Idle chat → 202 with spawned: true |
| `TestSendMessage_202_Stdin` | Running chat → 202 (injected) |
| `TestSendMessage_429_Busy` | Running + pending → 429 with retry_after_ms |
| `TestSendMessage_404_NotFound` | Unknown name → 404 |
| `TestGetChatMessages_200` | Returns events from session logs |
| `TestGetChatMessages_Pagination` | before cursor and limit respected |
| `TestUpdateChatConfig_200_Idle` | Idle chat config update succeeds |
| `TestUpdateChatConfig_409_Running` | Running chat → 409 |
| `TestDeleteChat_204` | Chat deleted → 204 |
| `TestDeleteChat_404` | Missing → 404 |

Use existing `httptest.NewRecorder` + gin test patterns from `pkg/api/api_test.go`.

**DoD:** All tests pass. Coverage ≥ 80% on `chat_handlers.go`.

---

## Phase 5: CLI Commands

**Goal:** Add `agentd chat` subcommands and update `agentd attach` to accept chat names.

### Task 5.1: `agentd chat` subcommand router

**File(s):** `cmd/agentd/chat.go` (new file), `cmd/agentd/main.go`

**What:**
Add `chat` subcommand to `main.go`:

```go
// main.go — add before port flag parsing
if len(os.Args) > 1 && os.Args[1] == "chat" {
    os.Exit(runChatCommand(os.Args[2:]))
}
```

`runChatCommand` routes to subcommands:

```go
func runChatCommand(args []string) int {
    if len(args) == 0 {
        fmt.Fprintln(os.Stderr, "Usage: agentd chat <create|send|list|delete> [options]")
        return 2
    }
    switch args[0] {
    case "create":
        return runChatCreate(args[1:])
    case "send":
        return runChatSend(args[1:])
    case "list":
        return runChatList(args[1:])
    case "delete":
        return runChatDelete(args[1:])
    default:
        fmt.Fprintf(os.Stderr, "agentd chat: unknown subcommand %q\n", args[0])
        return 2
    }
}
```

---

### Task 5.2: `agentd chat create`

**File(s):** `cmd/agentd/chat.go`

**What:**

```
agentd chat create <name> --agent=claude [--runtime=docker] [--model=opus]
    [--work-dir=/path] [--idle-timeout=30m] [--config=chat.yaml]
```

Flags:
- `--agent` — required, agent name
- `--runtime` — default empty (uses daemon default)
- `--model` — optional
- `--effort` — optional
- `--work-dir` — optional
- `--idle-timeout` — default "30m"
- `--config` — path to YAML file with full `ChatConfig` (mutually exclusive with individual flags)

If `--config` is given, unmarshal YAML into `ChatConfig` and POST directly. Otherwise, build config from flags.

Success output: prints `Created chat "{name}" (volume: {volumeName})`.

Uses `pkg/client` HTTP client (`GET http://localhost:{port}/chats`). Port flag: `--port` (default 8090).

**DoD:**
- `agentd chat create web-ui --agent=claude` creates chat and prints confirmation.
- `--config` path loads full YAML config.
- Duplicate name exits non-zero with clear error.

---

### Task 5.3: `agentd chat send`

**File(s):** `cmd/agentd/chat.go`

**What:**

```
agentd chat send <name> <message>
agentd chat send <name> --follow
```

Sends a message and optionally streams the response.

Flags:
- `--follow` / `-f` — after sending, attach to the WS stream and print output until session exits.
- `--port` — default 8090.

Behavior:
1. `POST /chats/{name}/messages {message: ...}` → get `SendMessageResponse`.
2. If `--follow`: connect to `ws_url` from response and stream (reuse `attach.go`'s `printNDJSON` logic).
3. Print session ID and state to stderr. Print response content to stdout if `--follow`.

If `429 Too Many Requests`: print `Chat is busy. Retry after {retry_after_ms}ms.` and exit 1.

**DoD:**
- `agentd chat send web-ui "hello world"` sends message, prints session ID.
- `agentd chat send web-ui "hello" --follow` streams response to stdout.
- Busy chat exits with non-zero exit code and useful message.

---

### Task 5.4: `agentd chat list`

**File(s):** `cmd/agentd/chat.go`

**What:**

```
agentd chat list [--json]
```

`GET /chats` → print table:

```
NAME          STATE    AGENT   SESSIONS  LAST ACTIVE
web-ui        running  claude  3         2026-03-19 14:32
code-review   idle     claude  1         2026-03-18 10:15
```

With `--json`: print raw JSON array.

**DoD:** Table and JSON modes work. Empty registry prints `No chats.` in table mode.

---

### Task 5.5: `agentd chat delete`

**File(s):** `cmd/agentd/chat.go`

**What:**

```
agentd chat delete <name> [--remove-volume]
```

`DELETE /chats/{name}?remove_volume={bool}`.

Flags:
- `--remove-volume` — also remove the Docker volume (default: false).

Success: prints `Deleted chat "{name}"`.

**DoD:**
- Default does not remove volume.
- `--remove-volume` flag passes `?remove_volume=true`.
- Unknown name exits non-zero.

---

### Task 5.6: `agentd attach` chat name resolution

**File(s):** `cmd/agentd/attach.go`

**What:**
Update `runAttachCommand` to also accept a chat name as the positional argument:

```go
// If the argument doesn't look like a UUID, try to resolve it as a chat name.
if !isUUID(sessionID) {
    chatResp, err := resolveChatSession(sessionID, *port)
    if err != nil {
        fmt.Fprintf(os.Stderr, "attach: %v\n", err)
        return 1
    }
    sessionID = chatResp.CurrentSession
}
```

`resolveChatSession` calls `GET /chats/{name}`. If `state != running`, prints `Chat "{name}" is not running (state: {state}).` and returns error.

**DoD:**
- `agentd attach web-ui` resolves to current session and attaches.
- Non-running chat exits with clear error message.
- UUID argument still works exactly as before (backward compatible).

---

### Task 5.7: CLI command tests

**File(s):** `cmd/agentd/chat_test.go` (new file)

**What:**
Integration-style tests using `httptest.NewServer` to stub the agentd API:

| Test | What it verifies |
|------|-----------------|
| `TestChatCreate_Success` | Correct POST body, success message printed |
| `TestChatCreate_Duplicate` | 409 response → non-zero exit, error message |
| `TestChatCreate_FromYAML` | `--config` flag loads YAML into request |
| `TestChatSend_Success` | POST, session ID printed |
| `TestChatSend_Busy` | 429 → non-zero exit, retry message |
| `TestChatList_Table` | GET, table printed with correct columns |
| `TestChatList_JSON` | `--json` flag prints raw JSON |
| `TestChatList_Empty` | No chats → "No chats." |
| `TestChatDelete_Default` | No volume param in URL |
| `TestChatDelete_RemoveVolume` | `?remove_volume=true` in URL |
| `TestAttach_ByName` | GET /chats/{name} called, session ID used |
| `TestAttach_ByNameNotRunning` | Non-running chat → error message |

**DoD:** All tests pass with `go test ./cmd/agentd/...`.

---

## Phase 6: Message History

**Goal:** `GET /chats/:name/messages` returns a unified view of all messages across the session chain.

### Task 6.1: ChatLogReader

**File(s):** `pkg/chat/log_reader.go` (new file)

**What:**
Reads session log files across a session chain and returns filtered, paginated events.

```go
type LogReader struct {
    logDir string // {dataDir}/logs
}

func NewLogReader(dataDir string) *LogReader

// ReadMessages reads events from the session chain and returns them in order.
// Applies event type filter, cursor (before), and limit.
func (r *LogReader) ReadMessages(
    chain []string,
    limit int,
    before int64, // global offset cursor (0 = from beginning)
    types []string, // event types to include (nil = all)
) ([]ChatMessageEntry, bool, error) // entries, hasMore, err
```

Implementation:
- For each session ID in `chain` (oldest first):
  - Open `{logDir}/{sessionID}.ndjson` (skip if absent — session may have used a different log name).
  - Parse each line as NDJSON. Each event has `{"type": ..., "data": {...}, "offset": ..., "timestamp": ...}`.
  - Assign a global offset as `chainIndex * 1e9 + lineOffset` for stable cross-session ordering.
  - Filter by `types` if non-nil.
- Apply `before` cursor (skip events with global offset >= before).
- Collect up to `limit + 1` entries. If `limit + 1` entries found, set `hasMore = true` and trim to `limit`.

**DoD:**
- Empty chain returns empty slice, no error.
- Events from multiple sessions are returned in order (older sessions first).
- `before` cursor enables backward pagination.
- Missing log files are silently skipped.

---

### Task 6.2: Log reader tests

**File(s):** `pkg/chat/log_reader_test.go` (new file)

**What:**

| Test | What it verifies |
|------|-----------------|
| `TestReadMessages_SingleSession` | Events from one session returned correctly |
| `TestReadMessages_MultiSession` | Events concatenated across chain |
| `TestReadMessages_Limit` | Only N entries returned |
| `TestReadMessages_HasMore` | `hasMore: true` when more entries exist |
| `TestReadMessages_BeforeCursor` | Events before cursor excluded |
| `TestReadMessages_TypeFilter` | Only matching event types returned |
| `TestReadMessages_MissingLogFile` | Missing file silently skipped |
| `TestReadMessages_EmptyChain` | Returns empty slice |

**DoD:** All tests pass.

---

## Phase 7: Daemon Wiring

**Goal:** Initialize the chat subsystem in `cmd/agentd/main.go` alongside existing session and runtime setup.

### Task 7.1: Daemon startup integration

**File(s):** `cmd/agentd/main.go`

**What:**
After session recovery, initialize and start the chat subsystem:

```go
// Initialize chat subsystem
chatDir := filepath.Join(*dataDir, "chats")
if err := os.MkdirAll(chatDir, 0755); err != nil {
    log.Fatalf("failed to create chat dir: %v", err)
}

chatRegistry := chat.NewRegistry(*dataDir)
chatManager := chat.NewManager(
    chatRegistry,
    sessions,
    runtimeMap, // map[string]runtime.Runtime built from rt + extraRuntimes
    *rtName,
)
chatWatcher := chat.NewIdleWatcher(chatRegistry, sessions, chatManager)

ctx, cancel := context.WithCancel(context.Background())
defer cancel()
chatWatcher.Start(ctx)

// Recover running chats: any chat with state=="running" whose session
// no longer exists in the recovered session set should be transitioned to idle.
recoverRunningChats(chatRegistry, chatManager, sessions)
```

`recoverRunningChats`:
```go
func recoverRunningChats(reg *chat.Registry, mgr *chat.Manager, sm *session.Manager) {
    chats, _ := reg.List()
    for _, c := range chats {
        if c.State != chat.ChatStateRunning {
            continue
        }
        if _, ok := sm.Get(c.CurrentSession); !ok {
            // Session not recovered — transition to idle
            c.State = chat.ChatStateIdle
            c.CurrentSession = ""
            _ = reg.Save(c)
            log.Printf("chat %q recovered to idle (session %s not found)", c.Name, c.CurrentSession)
        }
    }
}
```

Update `NewServer` call to pass chat components.

**DoD:**
- Daemon starts successfully with chat subsystem initialized.
- Running chats whose sessions did not recover are transitioned to idle on startup.
- `GET /chats` returns persisted chats after daemon restart.

---

### Task 7.2: Runtime map construction

**File(s):** `cmd/agentd/main.go`

**What:**
Currently the daemon constructs `rt` (primary runtime) and `extraRuntimes` (slice) separately. `ChatManager` needs a `map[string]runtime.Runtime` keyed by runtime name. Build this map:

```go
runtimeMap := map[string]runtime.Runtime{
    rt.Name(): rt,
}
for _, r := range extraRuntimes {
    runtimeMap[r.Name()] = r
}
```

This map is also used by `NewServer` for existing session dispatch. No behavioral change — just consolidates construction.

**DoD:** Daemon compiles and existing session dispatch works as before.

---

### Task 7.3: Daemon integration test

**File(s):** `pkg/e2e/chat_test.go` (new file, or extend existing `pkg/e2e/`)

**What:**
End-to-end test using a real local daemon (no Docker required — local runtime):

1. Start agentd with local runtime on a random port.
2. `POST /chats` with `agent=echo` (a minimal test agent that echoes input) — or use a mock agent.
3. `POST /chats/{name}/messages {message: "hello"}` → verify `202`.
4. `GET /chats/{name}` → verify `state = running`.
5. Wait for session to complete.
6. `GET /chats/{name}` → verify `state = idle`.
7. `POST /chats/{name}/messages {message: "hello again"}` → verify `202`, `spawned: true`.
8. `GET /chats/{name}/messages` → verify two message events.
9. `DELETE /chats/{name}` → verify `204`.
10. `GET /chats/{name}` → verify `404`.

**DoD:** Test passes against a real daemon binary.

---

## Phase 8: PAOP Integration

**Goal:** Connect PAOP's `persist chat` CLI and agentruntime executor to the agentd named chat API.

### Task 8.1: AgentRuntimeClient — chat API methods

**File(s):** `paop/executor/agentruntime_client.py`

**What:**
Add chat-related HTTP methods to the existing `AgentRuntimeClient`:

```python
async def create_chat(
    self,
    name: str,
    config: dict,
) -> dict:
    """POST /chats → ChatResponse"""

async def get_chat(self, name: str) -> dict:
    """GET /chats/{name} → ChatResponse. Raises 404 if not found."""

async def list_chats(self) -> list[dict]:
    """GET /chats → list[ChatSummary]"""

async def send_chat_message(self, name: str, message: str) -> dict:
    """POST /chats/{name}/messages → SendMessageResponse"""

async def delete_chat(
    self,
    name: str,
    remove_volume: bool = False,
) -> None:
    """DELETE /chats/{name}[?remove_volume=true]"""

async def update_chat_config(self, name: str, config: dict) -> dict:
    """PATCH /chats/{name}/config → ChatResponse"""

async def get_chat_messages(
    self,
    name: str,
    limit: int = 100,
    before: int | None = None,
) -> dict:
    """GET /chats/{name}/messages → ChatMessagesResponse"""
```

Follow existing patterns in `agentruntime_client.py`: use `self._session`, `self._base_url`, consistent `AgentRuntimeError` on non-2xx.

**DoD:**
- All methods follow existing client conventions (async, error handling, type hints).
- `AgentRuntimeError` raised on `4xx`/`5xx`.
- `send_chat_message` raises `ChatBusyError(retry_after_ms=...)` on `429`.

---

### Task 8.2: `persist chat` command — agentd integration

**File(s):** `paop/cli/chat.py`

**What:**
Update `_cmd_chat_launch()` to use the agentd named chat API when agentruntime is available (i.e., `AGENTRUNTIME_URL` is set or agentruntime daemon is healthy).

New flow for `persist chat [name]`:

1. Sanitize name: `cli-chat-{name}` → agentd chat name `{name}` (drop `cli-chat-` prefix for cleaner names).
2. Try `client.get_chat(name)`. If `404`, create the chat:
   ```python
   profile = ChatProfileResolver().resolve(name)  # existing profile resolution
   await client.create_chat(name, {
       "agent": profile.agent or "claude",
       "runtime": profile.runtime_sub or "docker",
       "model": profile.model,
       "work_dir": profile.work_dir,
       "idle_timeout": "30m",
   })
   ```
3. For interactive mode (no piped stdin): spawn `agentd chat send {name} --follow` or connect to `ws_url` directly for interactive terminal use.
4. For one-shot mode (piped input): `await client.send_chat_message(name, message)`.

Preserve existing tmux fallback: if agentruntime client is unavailable, fall back to current `_cmd_chat_launch_agentruntime()` / tmux flow.

**DoD:**
- `persist chat web-ui` creates or resumes the `web-ui` named chat via agentd.
- Chat profiles from `config/chat-profiles.yaml` continue to work.
- Fallback to tmux works when agentruntime is not available.

---

### Task 8.3: PAOP integration tests

**File(s):** `tests/test_chat_agentd_integration.py` (new file)

**What:**
Unit tests for the new agentd-backed `persist chat` path using mocked `AgentRuntimeClient`:

| Test | What it verifies |
|------|-----------------|
| `test_chat_launch_creates_chat_if_not_exists` | 404 → create_chat called |
| `test_chat_launch_reuses_existing_chat` | 200 → no create_chat call |
| `test_chat_launch_uses_profile_config` | Profile model/runtime passed to create |
| `test_chat_fallback_to_tmux_on_client_error` | Connection error → tmux path |
| `test_send_chat_message_busy_retry_hint` | ChatBusyError → user-visible message |

**DoD:** All tests pass with `uv run python -m pytest tests/test_chat_agentd_integration.py -v`.

---

## Testing Strategy Summary

| Layer | Coverage Target | Test Count (est.) |
|-------|----------------|-------------------|
| `pkg/chat` types + registry | 100% | 15 |
| `pkg/chat` manager | 90% | 14 |
| `pkg/chat` idle watcher | 90% | 6 |
| `pkg/api` chat handlers | 80% | 20 |
| `cmd/agentd` chat CLI | 85% | 13 |
| `pkg/chat` log reader | 90% | 8 |
| `pkg/e2e` daemon integration | key flows | 1 scenario |
| PAOP Python (`tests/`) | 85% | 5 |
| **Total** | | **~82 tests** |

---

## Non-Goals

- Chat-to-chat messaging or multi-agent chat (out of scope).
- Web UI for chat management (the dashboard can be extended separately using the REST API).
- Multi-user access control per chat (agentruntime is single-operator; trust tiers live in PAOP).
- Real-time notification when a chat transitions to idle (polling `GET /chats/:name` is sufficient).
- Automatic volume backup or migration to remote storage.
- Message search or full-text indexing (log files are the source of truth).
- Chat naming with Unicode characters (ASCII alphanumeric + hyphens + underscores only, for volume name compatibility).

---

## Security Considerations

1. **Chat name → volume name injection.** The chat name is validated with `ValidateName` before use in volume name construction. The validator rejects all characters that could cause command injection in `docker volume create`.
2. **Volume isolation.** Each named chat has its own volume (`agentruntime-chat-{name}`). No cross-chat volume access is possible.
3. **Config env passthrough.** `ChatConfig.Env` follows the same `writeDockerEnvFile` hardening as `SessionRequest.Env` — values containing raw newlines or NUL bytes are rejected.
4. **Pending message slot.** The single-pending-slot design limits memory exposure. Messages are stored in the JSON file, not in memory, so they survive daemon restart.
5. **`PATCH /chats/:name/config` idle gate.** Config updates (including MCP server URLs) are only permitted when the chat is idle, preventing a running agent from receiving unexpected tool access mid-conversation.

---

## Open Questions

1. **Effort field.** `ChatConfig.Effort` is passed as a `Tag` today (no direct Claude CLI flag). If effort-to-flag mapping is added to agentruntime, this field should wire through. For now, informational only.
2. **Local runtime resume.** When using `runtime: local`, Claude session files live in the materializer's host-side `SessionDir`. Identifying the correct `.jsonl` for `--resume` requires reading from the previous session's `SessionDir`. This is possible but adds complexity. **Decision for now:** local runtime resume works if `claude_session_id` tag was captured; otherwise, a fresh session starts with no history. Document this limitation.
3. **Interactive terminal UX.** `agentd chat send {name} --follow` streams output but is not a true interactive REPL. A proper interactive mode would require terminal raw mode and PTY allocation. This can be a follow-up (`agentd chat repl {name}` command).
