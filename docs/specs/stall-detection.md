# Spec: Two-Phase Stall Detection

**Status:** Draft
**Task:** #5
**Effort:** Medium (~200 LOC)
**Source:** persistence `executor/cli/process_runner.py`, `executor/docker_polling.py`

## Problem

Agentruntime relies solely on context cancellation for timeouts. If an agent hangs
(frozen API call, stuck tool, infinite loop), there's no detection until the hard
session timeout fires — which could be hours. No advisory logging, no graduated
response, no early exit when the agent has already emitted its result.

## Solution

Three-tier stall detection that monitors event stream activity:

1. **Result marker early exit** — break immediately when agent signals completion
2. **Advisory warning** — log after 10 min of no events (agent might be thinking)
3. **Hard kill** — terminate after 50 min of no events (agent is truly stuck)

## Design

### Event Stream Monitoring

The sidecar already receives all agent events. Track `lastEventTime` on every
event received. A background goroutine checks this timestamp periodically.

```go
type StallDetector struct {
    lastEventTime atomic.Value  // time.Time
    advisoryTimeout time.Duration  // default 10m
    hardTimeout     time.Duration  // default 50m
    resultSeen      atomic.Bool
}
```

### Result Marker Detection

When the normalizer sees `type: "result"` (Claude) or `type: "turn.completed"` (Codex),
set `resultSeen = true`. If the process hasn't exited after a 10-second grace period,
force-kill it — the agent finished but its process tree hung (common with MCP servers
that don't shut down cleanly).

### Phase Timeline

```
t=0        Agent starts
t=0..600s  Normal operation, no stall checks
t=600s     Advisory warning logged if no events since last check
t=3000s    Hard kill — session terminated, error event emitted
```

Timeouts configurable via session config:
```json
{
  "stall_advisory_timeout": 600,
  "stall_hard_timeout": 3000
}
```

## Implementation

### File: `cmd/sidecar/stall.go` (new)

```go
func NewStallDetector(advisory, hard time.Duration) *StallDetector
func (s *StallDetector) RecordEvent()       // called on every normalized event
func (s *StallDetector) RecordResult()      // called on result/turn.completed
func (s *StallDetector) Run(ctx context.Context, cancel context.CancelFunc)
```

### Wire into sidecar

In the WebSocket event loop (`cmd/sidecar/ws.go`), call `detector.RecordEvent()`
on every event from the agent backend. Call `detector.RecordResult()` on
result events. Start `detector.Run()` in a goroutine with the session context.

### Daemon-side awareness

Add `stall_status` to session metadata exposed via the daemon API:
- `"normal"` — events flowing
- `"quiet"` — advisory threshold passed
- `"stalled"` — hard timeout imminent

### Error event on hard kill

```json
{
  "type": "error",
  "data": {
    "message": "Agent stalled: no output for 3000s, session terminated",
    "error_category": "stall_timeout"
  }
}
```

## Testing

- Unit test: no events for advisory timeout → warning logged
- Unit test: no events for hard timeout → context cancelled
- Unit test: result event + 10s grace → force kill
- Integration test: spawn agent that hangs → verify stall detection fires
