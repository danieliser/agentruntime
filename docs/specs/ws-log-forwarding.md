# Spec: WebSocket Output Forwarding from Log File

**Status:** Draft
**Task:** #10
**Effort:** Medium (~200 LOC)
**Source:** persistence `executor/cli/process_runner.py` `_forward_session_output()`

## Problem

When a client disconnects and reconnects to a session WebSocket, it gets a replay
of buffered events. But if the replay buffer has been trimmed (long-running sessions),
the client misses events. There's no way to catch up from the NDJSON log file that
the daemon persists to disk.

Additionally, if the daemon restarts mid-session, the in-memory replay buffer is lost.
The NDJSON log file on disk is the only source of truth.

## Solution

A log-file tailing goroutine that reads the NDJSON session log and broadcasts
new events over the daemon's WebSocket bridge. This serves two purposes:

1. **Reconnection catch-up**: On client reconnect, read from the log file
   instead of (or in addition to) the in-memory replay buffer
2. **Daemon restart recovery**: After daemon restart, the log file provides
   full session history for replay

## Design

### Log File Tailing

```go
type LogForwarder struct {
    logPath     string
    lastOffset  int64
    sessionID   string
    broadcast   func(event []byte)
    pollInterval time.Duration  // default 250ms
}

func (lf *LogForwarder) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case <-time.After(lf.pollInterval):
            lf.forwardNewLines()
        }
    }
}
```

### Integration Points

#### 1. Replay on reconnect

In `pkg/bridge/`, when a client connects to `/ws/sessions/:id`:
- If replay buffer has full history → use buffer (fast path)
- If buffer is partial → read from NDJSON log file, then switch to live stream

#### 2. Daemon restart recovery

In `pkg/session/manager.go`, on startup:
- Scan session log directory for active sessions
- For each, create a LogForwarder to catch up from the log file
- Merge with live sidecar events once the sidecar reconnects

### NDJSON Log Path

Sessions already write NDJSON logs via the replay buffer's persistence layer.
Path: `{session_dir}/{session_id}.ndjson`

### Completion Detection

Stop tailing when:
- Session ends (exit event received from sidecar)
- Context cancelled (session deleted)
- No new data for 2 consecutive polls AND session is not active

### WebSocket Event Format

Forwarded events use the existing `stdout` message type:
```json
{
  "type": "replay",
  "data": "{\"type\":\"agent_message\",\"data\":{...},\"offset\":123}"
}
```

The `replay` type tells the client these are historical events, not live.

## Testing

- Client disconnect + reconnect → receives missed events from log
- Daemon restart → session history available from log file
- Large log file (>10MB) → tailing is efficient (seek-based, not full read)
- Session ends → forwarder stops cleanly
