# Codebase Analysis: WebSocket Log Forwarding for Reconnection

> Generated 2026-03-18 — covers `agentruntime` repo at `/Users/danieliser/Toolkit/agentruntime`

---

## 1. Architecture Overview

The agentruntime implements a two-tier output pipeline: an in-memory circular replay buffer plus persistent NDJSON log files. WebSocket clients stream output in real-time and can reconnect with offset-based replay.

```
Agent Process Stdout/Stderr
    ↓
Drain Goroutine (pkg/api/sessionio.go)
    ├→ ReplayBuffer.Write()   [in-memory, 1 MiB circular]
    └→ LogWriter.Write()      [persistent NDJSON on disk]
         ↓
         ~/.local/share/agentruntime/logs/{sessionID}.ndjson

Client connects via WebSocket:
    WS /ws/sessions/:id?since=<offset>
         ↓
    Bridge.replayStreamPump() → ReplayBuffer.WaitFor(offset)
         ↓
    ServerFrame{ type:"stdout", data:"...", offset:N }
```

---

## 2. Priority Area Deep Dive

### 2.1 Bridge Package (`pkg/bridge/`)

**Files**: `bridge.go` (279 lines), `frames.go` (56 lines)

**Bridge Struct** (`bridge.go:27-33`):
```go
type Bridge struct {
    conn    *websocket.Conn
    handle  runtime.ProcessHandle
    replay  *session.ReplayBuffer
    writeMu sync.Mutex
    cancel  context.CancelFunc
}
```

**Key Methods**:

| Method | Location | Purpose |
|--------|----------|---------|
| `New()` | `bridge.go:36` | Creates bridge; call `Run()` to start |
| `Run()` | `bridge.go:47` | Main entry; blocks until disconnect or process exit |
| `replayStreamPump()` | `bridge.go:111` | Subscribes to replay buffer via `WaitFor()`, sends stdout frames |
| `stdinPump()` | `bridge.go:166` | Reads client frames; routes stdin/steer/interrupt/context/mention |
| `pingLoop()` | `bridge.go:242` | Heartbeat keepalive (30s interval, 10s pong timeout) |
| `writeJSON()` | `bridge.go:273` | Mutex-protected frame write with 5s deadline |

**Reconnection Flow** (`bridge.go:47-70`):
```go
// Run() entry point — handles ?since= replay
sinceOffset := int64(-1)  // default: no replay
if sinceOffset >= 0 {
    data, nextOffset := b.replay.ReadFrom(sinceOffset)
    if len(data) > 0 {
        _ = b.writeJSON(ServerFrame{
            Type:   "replay",
            Data:   base64.StdEncoding.EncodeToString(data),
            Offset: nextOffset,
        })
    }
    streamOffset = nextOffset
} else {
    streamOffset = b.replay.TotalBytes()  // start from current
}
```

**Stream Pump Details** (`bridge.go:111-163`):
- Calls `replay.WaitFor(offset)` — blocks until new data or buffer closed
- Non-UTF8 data sent as base64 (`bridge.go:128-130`)
- Every frame includes `offset` for client cursor tracking
- Returns immediately on context cancellation (`bridge.go:122-124`)
- Sends exit frame with code from `handle.Wait()` (`bridge.go:145-158`)

**Timeouts** (`bridge.go:17-20`):
- `writeTimeout = 5s`
- `readTimeout = 60s`
- `pongTimeout = 10s`

**Write Serialization**: All writes protected by `writeMu.Lock()` (`bridge.go:274`). Single-writer pattern prevents frame corruption across pingLoop, replayStreamPump, and stdin error responses.

---

### 2.2 Frame Protocol (`pkg/bridge/frames.go`)

**ServerFrame** (server → client, `frames.go:8-20`):
```go
type ServerFrame struct {
    Type      string `json:"type"`                // stdout, stderr, exit, replay, connected, pong, error
    Data      string `json:"data,omitempty"`       // output text (base64 for binary)
    Offset    int64  `json:"offset,omitempty"`     // replay buffer position (monotonic)
    ExitCode  *int   `json:"exit_code,omitempty"`
    SessionID string `json:"session_id,omitempty"` // set on connected frame
}
```

**ClientFrame** (client → server, `frames.go:22-48`):
```go
type ClientFrame struct {
    Type    string          `json:"type"`    // stdin, ping, resize, steer, interrupt, context, mention
    Data    string          `json:"data,omitempty"`
    Context *ContextPayload `json:"context,omitempty"`
    Mention *MentionPayload `json:"mention,omitempty"`
}
```

**Frame Sequence on Connect**:
1. `connected` (session_id, mode)
2. `replay` (if `?since=` provided and data available)
3. `stdout` frames (streaming, each with offset)
4. `exit` (exit_code, final offset)

---

### 2.3 Session Package (`pkg/session/`)

**Files**: `session.go` (289 lines), `replay.go` (184 lines), `logfile.go` (84 lines)

**Session Struct** (`session.go:27-49`):
```go
type Session struct {
    ID          string
    TaskID      string
    AgentName   string
    RuntimeName string
    State       State                 // pending, running, completed, failed, orphaned
    ExitCode    *int
    CreatedAt   time.Time
    EndedAt     *time.Time
    Replay      *ReplayBuffer
    Handle      runtime.ProcessHandle

    // Metrics
    LastActivity  *time.Time
    InputTokens   int
    OutputTokens  int
    CostUSD       float64
    ToolCallCount int

    mu sync.Mutex
}
```

**Session Lifecycle** (`session.go:51-99`):
```
NewSession() → Pending
SetRunning(handle) → Running
SetCompleted(code) → Completed (code=0) or Failed (code≠0)
Recover() → Orphaned
```

**ReplayBuffer** (`replay.go:17-141`):
```go
type ReplayBuffer struct {
    mu    sync.Mutex
    cond  *sync.Cond
    buf   []byte      // circular storage
    size  int          // capacity (default 1 MiB)
    head  int          // next write position
    Total int64        // total bytes ever written (monotonic)
    done  bool         // set when EOF
}
```

| Method | Location | Behavior |
|--------|----------|----------|
| `Write(p)` | `replay.go:43` | Append to ring, broadcast to WaitFor |
| `WaitFor(offset)` | `replay.go:71` | Block until offset available or done |
| `ReadFrom(offset)` | `replay.go:143` | Non-blocking read from offset |
| `TotalBytes()` | `replay.go:134` | Get monotonic total position |
| `Close()` | `replay.go:125` | Mark done, wake all waiters |
| `LoadFromFile(path)` | `replay.go:172` | Restore from NDJSON log file |

**Offset Semantics** (`replay.go:143-169`):
```go
func (r *ReplayBuffer) ReadFrom(offset int64) ([]byte, int64) {
    oldest := r.Total - int64(r.size)  // oldest available
    if offset >= r.Total { return nil, r.Total }  // caught up
    if offset < oldest  { offset = oldest }        // silent truncation
    // Return bytes from offset to Total
}
```

Key invariant: `offset` is absolute byte position in the theoretical infinite stream. Never resets, monotonically increasing, even after circular buffer wraparound.

**LogWriter** (`logfile.go:18-84`):
```go
type LogWriter struct {
    file *os.File
    path string
}
```
- Path convention: `{logDir}/{sessionID}.ndjson` (legacy: `.jsonl`)
- Creates file on construction (`logfile.go:37-56`)
- Append-only, no locking (OS-level atomic for small writes)
- `DrainWriter(replay, logw)` returns `io.MultiWriter` writing to both targets

---

### 2.4 Session I/O (`pkg/api/sessionio.go`)

**AttachSessionIO** (`sessionio.go:15-60`):
```go
func AttachSessionIO(sess *session.Session, logDir string) {
    logw, _ := session.NewLogWriter(logDir, sess.ID)
    drainTarget := session.DrainWriter(sess.Replay, logw)  // MultiWriter

    drainTo(sess, "stdout", handle.Stdout(), drainTarget)
    drainTo(sess, "stderr", handle.Stderr(), drainTarget)

    go func() {
        result := <-handle.Wait()
        drainWg.Wait()
        sess.Replay.Close()
        logw.Close()
        sess.SetCompleted(result.Code)
    }()
}
```

**Event Parsing** (`sessionio.go:114-155`):
- Lines are NDJSON; sidecar events have `type` field
- Tracks: `tool_use` (tool call count), `result` (token usage)
- Updates: `RecordActivity()`, `RecordToolCall()`, `RecordUsage()`

---

## 3. API Conventions

### 3.1 Route Structure (`pkg/api/routes.go:6-21`)

```
POST   /sessions              → create session
GET    /sessions              → list sessions
GET    /sessions/:id          → get session state
GET    /sessions/:id/info     → detailed info (uptime, tokens)
GET    /sessions/:id/logs     → cursor-based log reader (HTTP polling)
GET    /sessions/:id/log      → full NDJSON file download
DELETE /sessions/:id          → kill session
GET    /ws/sessions/:id       → WebSocket bridge (?since= query)
```

### 3.2 Server Struct (`pkg/api/server.go:18-28`)

```go
type Server struct {
    router   *gin.Engine
    sessions *session.Manager
    runtimes map[string]runtime.Runtime
    runtime  runtime.Runtime
    agents   *agent.Registry
    dataDir  string
    logDir   string
    srv      *http.Server
}
```

### 3.3 Handler Patterns

**WebSocket Upgrade** (`handlers.go:318-347`):
```go
func (s *Server) handleSessionWS(c *gin.Context) {
    sess := s.sessions.Get(c.Param("id"))
    sinceOffset := int64(-1)
    if sinceStr := c.Query("since"); sinceStr != "" {
        parsed, _ := strconv.ParseInt(sinceStr, 10, 64)
        sinceOffset = parsed
    }
    conn, _ := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
    b := bridge.New(conn, sess.Handle, sess.Replay)
    ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
    b.Run(ctx, sess.ID, sinceOffset)
}
```

**HTTP Log Cursor** (`handlers.go:296-316`):
```go
func (s *Server) handleGetLogs(c *gin.Context) {
    cursor := int64(0)
    if cursorStr := c.Query("cursor"); cursorStr != "" {
        cursor, _ = strconv.ParseInt(cursorStr, 10, 64)
    }
    data, nextOffset := sess.Replay.ReadFrom(cursor)
    c.Header("Agentruntime-Log-Cursor", strconv.FormatInt(nextOffset, 10))
    c.Data(http.StatusOK, "text/plain", data)
}
```

**Log File Download** (`handlers.go:349-362`):
```go
func (s *Server) handleGetLogFile(c *gin.Context) {
    logPath, exists, _ := session.ExistingLogFilePath(s.logDir, id)
    c.Header("Content-Type", "application/x-ndjson")
    c.File(logPath)
}
```

### 3.4 Error Conventions

| Status | When |
|--------|------|
| `201 Created` | POST /sessions success |
| `200 OK` | GET success |
| `400 Bad Request` | Validation error |
| `404 Not Found` | Session/log not found |
| `409 Conflict` | Duplicate session_id, no active process |
| `503 Service Unavailable` | Max sessions reached |

WebSocket errors: `{"type":"error","error":"<message>"}`

---

## 4. Daemon Entrypoint (`cmd/agentd/main.go`)

**Startup** (`main.go:23-117`):
1. Parse flags: `--port`, `--runtime`, `--data-dir`, `--credential-sync`, `--max-sessions`
2. Initialize runtime (local, docker, or local-pipe)
3. Initialize SessionManager
4. Recover orphaned sessions from prior daemon run
5. Initialize agent registry
6. Create API server
7. Start HTTP server (blocks)
8. Graceful shutdown: SIGINT/SIGTERM → `srv.Shutdown()` → `rt.Cleanup()`

**Recovery Flow** (`main.go:57-71`):
```go
recovered, _ := rt.Recover(context.Background())
orphaned := sessions.Recover(recovered, rt.Name())
restoreRecoveredSessions(logDir, orphaned)

// restoreRecoveredSessions:
// 1. Load session NDJSON log file into Replay buffer
// 2. Attach stdio to process handle
// 3. Continue draining to log file
```

**Data Directory**:
```
~/.local/share/agentruntime/  (or AGENTRUNTIME_DATA_DIR)
├── logs/
│   ├── {sessionID}.ndjson
│   └── {sessionID}.jsonl     (legacy)
├── agent-sessions/
└── credentials/
```

---

## 5. Concurrency Model

### Per Session:
- **Drain goroutines** (stdout, stderr): write to replay + log continuously
- **Exit watcher**: waits on `handle.Wait()`, closes replay, transitions state
- **Bridge I/O** (on client connect):
  - `pingLoop`: periodic keepalives
  - `replayStreamPump`: subscribes to replay, sends frames
  - `stdinPump`: reads client frames, routes to handle

### Sync Primitives:

| Pattern | Location | Purpose |
|---------|----------|---------|
| `sync.Mutex` | `session.go:48` | Session state updates |
| `sync.Cond` | `replay.go:19` | Broadcast on Write, WaitFor blocks |
| `sync.Mutex` | `bridge.go:31` | Write serialization |
| `sync.WaitGroup` | `bridge.go:85` | Goroutine coordination |
| `sync.Once` | `wshandle.go:70` | One-time finish on disconnect |

### Context Usage:
- `context.WithCancel()`: bridge I/O loops (`bridge.go:48`)
- `context.WithTimeout()`: daemon shutdown (5s), bridge lifetime (24h)
- `<-ctx.Done()` select for graceful cancellation — never polling

---

## 6. Shutdown & Cleanup

**Daemon** (`main.go:93-108`):
```go
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
srv.Shutdown(ctx)
rt.Cleanup(ctx)
```

**Session Exit** (`sessionio.go:46-59`):
```go
result := <-handle.Wait()
drainWg.Wait()
sess.Replay.Close()   // unblock WaitFor callers
logw.Close()           // flush and close NDJSON
sess.SetCompleted(result.Code)
```

**Bridge** (`bridge.go:47-107`):
- `cancel()` wakes `replayStreamPump` from `WaitFor`
- `defer conn.Close()` closes WebSocket on exit
- Goroutines exit on context cancellation or read/write failure

---

## 7. Test Patterns

### Mock Handles (`pkg/bridge/bridge_integration_test.go:29-98`)
```go
type mockHandle struct {
    stdinR, stdinW   *io.PipeReader/*io.PipeWriter
    stdoutR, stdoutW *io.PipeReader/*io.PipeWriter
    stderrR, stderrW *io.PipeReader/*io.PipeWriter
    done             chan runtime.ExitResult
    pid              int
}
```

### Test Structure:
1. Create mock handle with pipes
2. Start drain goroutines (pipes → replay)
3. `httptest.Server` with bridge handler
4. Dial WebSocket, send/receive frames
5. Simulate process exit by closing pipes
6. Assert frame sequences (connected → replay/stdout → exit)

### Coverage:
- `TestReplayOnReconnect` — offset tracking across disconnect
- `TestServerFrame_JSONRoundtrip` — frame serialization
- Non-UTF8 data (base64 encoding)
- stdin forwarding
- Graceful disconnect

### Recovery Tests (`cmd/agentd/main_test.go:46-77`):
1. Write prior output to NDJSON log file
2. Create recovered session with logPath
3. Call `restoreRecoveredSessions()`
4. Assert `Replay.ReadFrom(0)` returns prior output
5. Assert new live output appended

---

## 8. Dependencies (`go.mod`)

| Dependency | Version | Use |
|---|---|---|
| `github.com/gin-gonic/gin` | v1.10.0 | HTTP framework |
| `github.com/google/uuid` | v1.6.0 | Session IDs |
| `github.com/gorilla/websocket` | v1.5.3 | WebSocket I/O |
| `github.com/creack/pty` | v1.1.24 | PTY support (future) |
| `gopkg.in/yaml.v3` | v3.0.1 | Config parsing |

---

## 9. Error Scenarios & Edge Cases

### Process Exits During WS Connection
1. Replay buffer already filled from drains
2. `Replay.Close()` wakes `WaitFor` callers — returns `(nil, Total, true)`
3. Bridge sends exit frame with code, cancels context
4. Client receives exit frame, knows connection will close

### WS Disconnect During Streaming
1. `conn.ReadJSON()` fails → stdinPump exits
2. replayStreamPump continues until buffer closed or context cancelled
3. Client reconnects with `?since=lastOffset`

### Stale Offset (Buffer Wraparound)
- Client disconnects for extended period
- New output wraps past old offset in 1 MiB buffer
- `ReadFrom()` silently advances to oldest available (`replay.go:158-160`)
- Client observes offset jump in replay frame
- **Gap**: no explicit signal that data was lost between disconnect and reconnect

### Log File Creation Failure
- Warning logged, but session continues with replay-only (`handlers.go:20-23`)
- Degrades gracefully: real-time streaming works, but no persistent recovery

---

## 10. Current Gaps & Missing Pieces

### For Full Reconnection Recovery:

1. **No log-file-based replay on reconnect**: When the replay buffer has wrapped past the client's offset, the NDJSON log file on disk has the complete history but isn't used for WS reconnection catch-up. `ReadFrom()` silently truncates.

2. **No data-loss signal**: Client has no way to know output was missed between disconnect and reconnect. The offset jump is the only implicit indicator.

3. **No daemon-restart recovery for WS clients**: On daemon restart, `restoreRecoveredSessions()` reloads the NDJSON file into the replay buffer (up to 1 MiB), but reconnecting clients have no protocol to discover a restart happened.

4. **No reconnection metrics**: No tracking of reconnect count, bytes replayed, or catch-up duration.

5. **No log file rotation/cleanup**: NDJSON files persist indefinitely. No mechanism for archival or pruning.

6. **No client-side offset persistence**: The protocol relies entirely on the client tracking its offset in memory. If the client process restarts, it has no way to recover its position (short of starting from 0 and replaying the full log).

---

## 11. Integration Points for the Feature

### Where reconnection logic lives:
- **Query parsing**: `pkg/api/handlers.go:337-341`
- **Replay delivery**: `pkg/bridge/bridge.go:55-70`
- **Stream pump**: `pkg/bridge/bridge.go:111-163`
- **Buffer API**: `pkg/session/replay.go:143-169`

### Where log-file-based catch-up would plug in:
- `pkg/bridge/bridge.go:Run()` — when `ReadFrom()` indicates truncation, fall back to reading the NDJSON file from disk
- `pkg/session/logfile.go` — add offset-aware read from NDJSON (currently only has `LoadFromFile` for full reload)
- `pkg/api/sessionio.go` — potentially track byte offsets in the log file alongside replay buffer offsets

### Where daemon restart recovery would extend:
- `cmd/agentd/main.go:restoreRecoveredSessions()` — already loads NDJSON into replay buffer
- `pkg/bridge/bridge.go:Run()` — needs to handle reconnect to a recovered session (replay buffer may have been reloaded from disk)

### Where new files should go (based on existing structure):
- Log reader utilities: `pkg/session/logreader.go`
- Protocol extensions: `pkg/bridge/frames.go` (new frame types if needed)
- Test coverage: `pkg/bridge/bridge_integration_test.go`, `pkg/session/logfile_test.go`
