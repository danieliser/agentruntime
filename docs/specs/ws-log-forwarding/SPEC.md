# Spec: WebSocket Log Forwarding — NDJSON-Based Catch-Up and Recovery

**Status:** Draft
**Task:** #10
**Effort:** Medium (~250 LOC across 4 files)

---

## Problem

The daemon's WebSocket bridge replays missed output from a 1 MiB circular replay buffer. Two scenarios break this:

1. **Buffer wraparound on reconnect.** A client disconnects from a long-running session, output exceeds the 1 MiB ring, and the client's `?since=` offset has been evicted. `ReadFrom()` silently advances to the oldest available byte — the client permanently loses intermediate output with no indication that a gap occurred.

2. **Daemon restart.** The in-memory replay buffer is destroyed. `restoreRecoveredSessions()` reloads the NDJSON log into a fresh replay buffer (capped at 1 MiB), but if the log is larger, early output is lost. Reconnecting clients also have no protocol signal that a restart occurred and offsets were reset.

Meanwhile, the complete session history already exists on disk as an append-only NDJSON log file (`{logDir}/{sessionID}.ndjson`). This file is never consulted during WebSocket reconnection.

## Solution

Extend the bridge reconnection path to fall back to the NDJSON log file when the replay buffer cannot satisfy the client's requested offset. Add a protocol-level signal (`gap` flag) so clients know when output was unavailable from any source.

Three changes:

1. **Log file reader** (`pkg/session/logreader.go`) — seek-and-stream reader that serves bytes from a given file offset, translating between replay-buffer byte offsets and log file byte positions.
2. **Bridge fallback** (`pkg/bridge/bridge.go`) — when `ReadFrom()` would truncate, read from the log file instead. Emit a `gap` flag on the replay frame if even the log file cannot cover the full range.
3. **Protocol extension** (`pkg/bridge/frames.go`) — add `Gap` field to `ServerFrame` so clients can detect data loss.

---

## Design

### Offset Model

The replay buffer tracks a monotonic `Total` — the count of bytes ever written. The NDJSON log file on disk receives the same bytes (via `io.MultiWriter`), so **file size == `Total` at any point in time** (assuming no write errors). This identity is the foundation of log-based catch-up: a client's `since` offset maps directly to a byte position in the log file.

```
Replay buffer (1 MiB ring):
  oldest available = Total - bufferSize
  ReadFrom(offset) works when offset >= oldest

NDJSON log file:
  file size == Total (always)
  ReadAt(offset) works when offset >= 0 and offset < fileSize
```

When `offset < oldest` (buffer can't satisfy), fall back to the log file. When `offset < 0` or the log file doesn't exist, the data is unrecoverable — signal a gap.

### Log File Reader

New file: `pkg/session/logreader.go`

```go
// ReadLogRange reads bytes from the NDJSON log file in [offset, endOffset).
// Returns the bytes read and any error. If the file is shorter than endOffset,
// returns whatever is available up to EOF.
func ReadLogRange(logDir, sessionID string, offset, endOffset int64) ([]byte, error)
```

Implementation:
1. Resolve the log file path via `ExistingLogFilePath(logDir, sessionID)`.
2. Open the file read-only.
3. Seek to `offset`.
4. Read up to `endOffset - offset` bytes.
5. Return the data. If the file is shorter than expected, return what's available — the caller handles the gap.

This is a stateless utility function — no goroutines, no tailing, no polling. The bridge calls it synchronously during the reconnection handshake, before starting the live stream pump.

### Bridge Reconnection Flow

Modified: `pkg/bridge/bridge.go` — `Run()` method.

Current flow (lines 55-70):
```
sinceOffset >= 0 → ReadFrom(sinceOffset) → send replay frame → start stream pump
```

New flow:
```
sinceOffset >= 0:
  1. Check replay buffer: data, nextOffset = replay.ReadFrom(sinceOffset)
  2. If data covers the full range (sinceOffset == oldest or buffer had it):
       → send replay frame (unchanged fast path)
  3. If sinceOffset < oldest available in buffer:
       → call ReadLogRange(logDir, sessionID, sinceOffset, oldest)
       → send log-sourced data as replay frame
       → then send remaining buffer data as second replay frame
       → set Gap=true on first frame if log file couldn't cover full range
  4. Start stream pump from nextOffset (unchanged)
```

The bridge needs access to `logDir` and `sessionID` to perform the log file fallback. These are passed through the constructor:

```go
func New(conn *websocket.Conn, handle runtime.ProcessHandle, replay *session.ReplayBuffer,
         logDir string, sessionID string) *Bridge
```

### Detecting Truncation

The replay buffer's `ReadFrom()` currently performs silent truncation — it advances `offset` to `oldest` without signaling. To enable the fallback path, the bridge needs to know whether truncation occurred.

Option A: Add a `ReadFromWithStatus()` method that returns whether truncation happened.
Option B: Check the condition in the bridge before calling `ReadFrom()`.

**Chosen: Option B** — the bridge already has `sinceOffset` and can call `TotalBytes()` and compute `oldest = Total - size`. This avoids changing the replay buffer API and keeps the detection logic in the bridge where it's acted upon.

```go
total := b.replay.TotalBytes()
oldest := total - int64(b.replay.Cap())
if oldest < 0 {
    oldest = 0
}
truncated := sinceOffset < oldest
```

The replay buffer needs a `Cap() int` accessor to expose its capacity:

```go
func (r *ReplayBuffer) Cap() int {
    return r.size
}
```

### Protocol Extension

Modified: `pkg/bridge/frames.go`

```go
type ServerFrame struct {
    Type      string `json:"type"`
    Data      string `json:"data,omitempty"`
    ExitCode  *int   `json:"exit_code,omitempty"`
    Offset    int64  `json:"offset,omitempty"`
    SessionID string `json:"session_id,omitempty"`
    Mode      string `json:"mode,omitempty"`
    Error     string `json:"error,omitempty"`
    Gap       bool   `json:"gap,omitempty"`        // NEW: true if output was lost
}
```

The `Gap` field is set on `replay` frames when the log file could not fully cover the requested offset range. This happens when:
- The log file doesn't exist (e.g., log creation failed at session start).
- The log file is shorter than expected (write error, filesystem issue).
- `sinceOffset` is negative or otherwise unresolvable.

A `gap: true` replay frame tells the client: "You missed some output that we cannot recover. The data in this frame starts from the earliest available point."

The `Gap` field uses `omitempty` so it's absent (not `false`) in the common case, maintaining backward compatibility with existing clients.

### Daemon Restart Recovery

The existing `restoreRecoveredSessions()` in `cmd/agentd/main.go` already handles the critical path:
1. Load NDJSON log into the replay buffer via `LoadFromFile()`.
2. Reattach stdout/stderr drains with `AttachSessionIO()`.

After this spec is implemented, reconnecting clients benefit automatically:
- If their `since` offset falls within the reloaded replay buffer → served from memory (fast path).
- If the replay buffer couldn't hold the entire log → the new fallback reads from the log file on disk.

No changes to `main.go` are required. The bridge fallback handles it transparently.

### Connected Frame Extension

Add `Recovered` field to the `connected` frame to signal that the session was recovered after a daemon restart:

```go
type ServerFrame struct {
    // ... existing fields ...
    Recovered bool `json:"recovered,omitempty"` // NEW: session was recovered after daemon restart
}
```

The handler sets this when the session's state is `StateOrphaned`:

```go
_ = b.writeJSON(ServerFrame{
    Type:      "connected",
    SessionID: sessionID,
    Mode:      "pipe",
    Recovered: sess.State == session.StateOrphaned,
})
```

This requires the bridge to receive the session state, which can be passed as a parameter to `Run()` or inferred from the session object. The simplest approach is to add a `recovered bool` parameter to `Run()`.

---

## Implementation Plan

### Step 1: `pkg/session/logreader.go` (new file, ~40 LOC)

```go
package session

import (
    "fmt"
    "io"
    "os"
)

// ReadLogRange reads bytes from the session's NDJSON log file in [offset, endOffset).
// Returns the data read. If the file is shorter than endOffset, returns available data.
// Returns an error if the file cannot be opened or read.
func ReadLogRange(logDir, sessionID string, offset, endOffset int64) ([]byte, error) {
    logPath, exists, err := ExistingLogFilePath(logDir, sessionID)
    if err != nil {
        return nil, fmt.Errorf("log file lookup: %w", err)
    }
    if !exists {
        return nil, fmt.Errorf("log file not found for session %s", sessionID)
    }

    f, err := os.Open(logPath)
    if err != nil {
        return nil, fmt.Errorf("open log file: %w", err)
    }
    defer f.Close()

    if offset < 0 {
        offset = 0
    }

    size := endOffset - offset
    if size <= 0 {
        return nil, nil
    }

    buf := make([]byte, size)
    n, err := f.ReadAt(buf, offset)
    if err != nil && err != io.EOF {
        return buf[:n], fmt.Errorf("read log file: %w", err)
    }
    return buf[:n], nil
}
```

### Step 2: `pkg/session/replay.go` (~5 LOC)

Add capacity accessor:

```go
// Cap returns the buffer capacity in bytes.
func (r *ReplayBuffer) Cap() int {
    return r.size
}
```

### Step 3: `pkg/bridge/frames.go` (~2 LOC)

Add fields to `ServerFrame`:

```go
Gap       bool   `json:"gap,omitempty"`
Recovered bool   `json:"recovered,omitempty"`
```

### Step 4: `pkg/bridge/bridge.go` (~60 LOC)

Update constructor signature:

```go
func New(conn *websocket.Conn, handle runtime.ProcessHandle, replay *session.ReplayBuffer,
         logDir string, sessionID string) *Bridge {
    return &Bridge{
        conn:      conn,
        handle:    handle,
        replay:    replay,
        logDir:    logDir,
        sessionID: sessionID,
    }
}
```

Add `logDir` and `sessionID` fields to the `Bridge` struct.

Update `Run()` reconnection logic:

```go
if sinceOffset >= 0 {
    total := b.replay.TotalBytes()
    bufCap := int64(b.replay.Cap())
    oldest := total - bufCap
    if oldest < 0 {
        oldest = 0
    }

    if sinceOffset >= oldest {
        // Fast path: buffer can satisfy the offset.
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
        // Slow path: buffer wrapped past client offset. Try log file.
        gap := false
        logData, err := session.ReadLogRange(b.logDir, b.sessionID, sinceOffset, oldest)
        if err != nil || len(logData) == 0 {
            gap = true
            log.Printf("[bridge %s] log catch-up failed from offset %d: %v", b.sessionID, sinceOffset, err)
        }
        if len(logData) > 0 {
            _ = b.writeJSON(ServerFrame{
                Type:   "replay",
                Data:   base64.StdEncoding.EncodeToString(logData),
                Offset: oldest,
                Gap:    gap,
            })
        }
        // Now send whatever the buffer has (from oldest onward).
        data, nextOffset := b.replay.ReadFrom(oldest)
        if len(data) > 0 {
            _ = b.writeJSON(ServerFrame{
                Type:   "replay",
                Data:   base64.StdEncoding.EncodeToString(data),
                Offset: nextOffset,
            })
        }
        if len(logData) == 0 && gap {
            // Log file unavailable — send a single gap replay from buffer.
            _ = b.writeJSON(ServerFrame{
                Type:   "replay",
                Data:   base64.StdEncoding.EncodeToString(data),
                Offset: nextOffset,
                Gap:    true,
            })
        }
        streamOffset = nextOffset
    }
} else {
    streamOffset = b.replay.TotalBytes()
}
```

### Step 5: `pkg/api/handlers.go` (~5 LOC)

Update the `bridge.New()` call in `handleSessionWS()`:

```go
b := bridge.New(conn, sess.Handle, sess.Replay, s.logDir, sess.ID)
```

### Step 6: Update call sites

Any other callers of `bridge.New()` (tests, integration tests) must be updated to pass the two new parameters. For test code, empty strings are acceptable — they disable the log fallback gracefully (the `ReadLogRange` call will return an error, `gap` will be set, and the buffer-only path will be used).

---

## Frame Sequence Examples

### Normal reconnect (buffer has full range)

```
Client connects: /ws/sessions/:id?since=50000
Server:
  { "type": "replay", "data": "base64...", "offset": 120000 }
  { "type": "connected", "session_id": "abc-123", "mode": "pipe" }
  { "type": "stdout", "data": "...", "offset": 120100 }
  ...
```

### Reconnect with log fallback (buffer wrapped)

```
Client connects: /ws/sessions/:id?since=50000
Buffer oldest = 500000, Total = 1500000

Server:
  { "type": "replay", "data": "base64(log bytes 50000..500000)", "offset": 500000 }
  { "type": "replay", "data": "base64(buffer bytes 500000..1500000)", "offset": 1500000 }
  { "type": "connected", "session_id": "abc-123", "mode": "pipe" }
  { "type": "stdout", "data": "...", "offset": 1500100 }
  ...
```

### Reconnect with gap (log file unavailable)

```
Client connects: /ws/sessions/:id?since=50000
Buffer oldest = 500000, log file missing

Server:
  { "type": "replay", "data": "base64(buffer bytes 500000..1500000)", "offset": 1500000, "gap": true }
  { "type": "connected", "session_id": "abc-123", "mode": "pipe" }
  ...
```

### Reconnect after daemon restart (recovered session)

```
Client connects: /ws/sessions/:id?since=0
Replay buffer reloaded from log (last 1 MiB)

Server:
  { "type": "replay", "data": "base64...", "offset": 1048576 }
  { "type": "connected", "session_id": "abc-123", "mode": "pipe", "recovered": true }
  ...
```

---

## Edge Cases

### Log file creation failed at session start
`AttachSessionIO` logs a warning and continues with replay-only mode. On reconnect, if the buffer has wrapped, the fallback `ReadLogRange` returns an error. The bridge sends a `gap: true` replay frame from whatever the buffer has. This is the same degraded behavior as today, but now the client knows about it.

### Client sends `since=0` on a fresh session
The replay buffer has the complete history (hasn't wrapped yet). Fast path is used. No log file access.

### Client sends `since=0` on a massive session (10 GB log)
The log file read is bounded: `ReadLogRange(dir, id, 0, oldest)` where `oldest` is `Total - 1MiB`. This reads `oldest` bytes from disk. For a 10 GB session, this means reading ~10 GB minus 1 MiB from the log file into memory.

**Mitigation:** For the initial implementation, this is acceptable — reconnection is not a hot path, and the data must reach the client regardless. A future optimization could stream the log data in chunks rather than buffering it all in memory, or cap the catch-up window and always set `gap: true` beyond a configurable limit.

### Concurrent log writes during read
The NDJSON log is append-only. `ReadAt` on a file being appended to is safe on Linux/macOS — it returns data up to the point of the read. The bridge reads up to `oldest` (a snapshot value), so the read range is already finalized in the log file.

### Daemon restart with no log file
Session is recovered with an empty replay buffer. Client connects with `since=0`, gets an empty replay, then receives live output. No gap signal needed because `since=0` and buffer `oldest=0`.

### Multiple concurrent reconnections
Each bridge instance is independent. Each performs its own `ReadLogRange` call with its own file handle. No shared state, no contention beyond filesystem I/O.

---

## Testing

### Unit tests: `pkg/session/logreader_test.go`

1. **Read full range** — write known content to a temp file, `ReadLogRange(0, fileSize)` returns all content.
2. **Read partial range** — `ReadLogRange(100, 200)` returns bytes 100-199.
3. **Offset beyond file** — `ReadLogRange(fileSize+1, fileSize+100)` returns empty.
4. **File not found** — returns error, no panic.
5. **Zero-length range** — `ReadLogRange(50, 50)` returns nil.

### Unit tests: `pkg/session/replay_test.go`

6. **Cap() accessor** — `NewReplayBuffer(4096).Cap() == 4096`.

### Integration tests: `pkg/bridge/bridge_integration_test.go`

7. **Reconnect within buffer** — write < 1 MiB, disconnect, reconnect with `since=0` → full replay, no gap.
8. **Reconnect with log fallback** — write > 1 MiB, create matching log file on disk, reconnect with `since=0` → log data + buffer data, no gap.
9. **Reconnect with gap** — write > 1 MiB, no log file, reconnect with `since=0` → partial replay with `gap: true`.
10. **Gap field absent on normal replay** — verify `gap` is not serialized when false (omitempty).

### Manual testing

11. Start a long-running interactive session. Disconnect the WS client. Wait for substantial output (>1 MiB). Reconnect with `?since=<old_offset>`. Verify the full catch-up is delivered from the log file.
12. Kill and restart `agentd` while a Docker session is running. Reconnect. Verify `recovered: true` on the connected frame and full log replay.

---

## Future Work

- **Streaming log reads**: For very large sessions, chunk the log file read and send multiple replay frames to avoid loading gigabytes into memory at once.
- **Configurable catch-up cap**: Add a max catch-up size (e.g., 50 MiB) beyond which the bridge always sends `gap: true` and starts from the tail.
- **Log file rotation/cleanup**: NDJSON files persist indefinitely today. Add TTL-based cleanup for completed sessions.
- **Client-side offset persistence**: SDK support for persisting the last-seen offset to enable recovery after client process restarts.
- **Reconnection metrics**: Track reconnect count, bytes replayed from log vs. buffer, and catch-up latency on the session object.
