# Two-Phase Stall Detection

**Status:** Draft
**Author:** agentruntime team
**Date:** 2026-03-19

## 1. Problem Statement

The sidecar has zero event-stream activity monitoring today. If an agent process hangs — whether due to an MCP server blocking exit, a stuck API call, or a process deadlock — the sidecar and daemon have no mechanism to detect this, warn operators, or reclaim resources. The `exitLoop()` in `cmd/sidecar/ws.go:398` blocks indefinitely on `backend.Wait()`, meaning a hung agent process lives forever.

A second failure mode occurs after the agent emits a `result` event (signaling turn completion) but the process fails to exit. This happens when MCP servers, subprocesses, or cleanup hooks prevent clean shutdown. The process is functionally done but consumes resources indefinitely.

## 2. Goals

1. **Result marker early exit:** Detect `result` events and force-kill the process if it hasn't exited within a 10-second grace period.
2. **Advisory warning:** Emit a `system` event after 10 minutes of event-stream silence so operators and monitoring can react.
3. **Hard kill:** Force-kill the agent process after 50 minutes of event-stream silence to reclaim resources.
4. **Configurable:** All thresholds are overridable via `AGENT_CONFIG` and have sensible defaults.
5. **Minimal blast radius:** The stall monitor runs only in the v2 `ExternalWSServer`. The legacy v1 PTY sidecar and the daemon are out of scope.

## 3. Non-Goals

- Daemon-side stall monitoring (the daemon already tracks `LastActivity` per session; a daemon-level watchdog is a separate feature).
- Stall detection in the legacy v1 PTY sidecar (`cmd/sidecar/legacy_v1.go`).
- Differentiating "useful" vs "useless" activity — any event resets the timer.
- Automatic retry of stalled sessions (retry is the daemon's responsibility; this spec provides the signal).

## 4. Design Overview

A single new goroutine, `stallMonitor()`, is launched from `ensureStarted()` alongside the existing `eventLoop()` and `exitLoop()`. It uses wall-clock comparison against an atomically-updated `lastEventTime` timestamp to detect silence, and a separate result-seen flag to detect post-result hangs.

```
ensureStarted()
  ├── go eventLoop()     ← updates lastEventTime on every event
  │                      ← sets resultSeen on "result" events
  ├── go exitLoop()      ← unchanged, handles normal exit
  └── go stallMonitor()  ← NEW: periodic tick checks
       ├── if resultSeen && now - resultSeenAt > gracePeriod  → force-kill
       ├── if now - lastEventTime > warningTimeout            → emit stall_warning
       └── if now - lastEventTime > killTimeout               → emit stall_kill, force-kill
```

## 5. Configuration

### 5.1 AgentConfig Fields

Three new fields are added to `AgentConfig` in `cmd/sidecar/agentconfig.go`:

```go
type AgentConfig struct {
    // ... existing fields ...

    // StallWarningTimeout is seconds of event-stream silence before emitting
    // an advisory stall_warning system event. Default: 600 (10 min). 0 = disabled.
    StallWarningTimeout int `json:"stall_warning_timeout,omitempty"`

    // StallKillTimeout is seconds of event-stream silence before force-killing
    // the agent process. Default: 3000 (50 min). 0 = disabled.
    StallKillTimeout int `json:"stall_kill_timeout,omitempty"`

    // ResultGracePeriod is seconds to wait after a result event for the process
    // to exit before force-killing. Default: 10. 0 = disabled.
    ResultGracePeriod int `json:"result_grace_period,omitempty"`
    // NOTE: -1 means "use default" is NOT needed; zero-value omitempty handles
    // the "not set" case. The constructor applies defaults when the value is 0.
}
```

Integer seconds are chosen over duration strings for consistency with existing patterns (`MaxTurns` is a plain `int`; `SIDECAR_CLEANUP_TIMEOUT` accepts integer seconds as its primary format).

### 5.2 Defaults

| Parameter | Default | Rationale |
|-----------|---------|-----------|
| `stall_warning_timeout` | 600s (10 min) | Matches PAOP prior art. Long enough to avoid false positives during large tool calls. |
| `stall_kill_timeout` | 3000s (50 min) | Matches PAOP prior art (5x warning). Generous to avoid killing slow-but-working agents. |
| `result_grace_period` | 10s | Process cleanup should complete in <10s. MCP server teardown is the typical blocker. |

### 5.3 Disabling

Setting any timeout to `-1` disables that specific detection phase. Setting all three to `-1` disables stall detection entirely. A value of `0` means "use default."

### 5.4 Example AGENT_CONFIG

```json
{
  "model": "claude-opus-4-5",
  "stall_warning_timeout": 300,
  "stall_kill_timeout": 1800,
  "result_grace_period": 15
}
```

## 6. Detailed Design

### 6.1 Activity Tracking

A new `atomic.Int64` field on `ExternalWSServer` stores the last event timestamp as Unix nanoseconds:

```go
type ExternalWSServer struct {
    // ... existing fields ...

    // Stall detection state.
    lastEventNano atomic.Int64  // Unix nanos of last event; updated in eventLoop
    resultSeen    atomic.Bool   // true after first "result" event
    resultNano    atomic.Int64  // Unix nanos when resultSeen became true

    // Stall detection config (set at construction, read-only after).
    stallWarningTimeout time.Duration
    stallKillTimeout    time.Duration
    resultGracePeriod   time.Duration
}
```

Atomics are used instead of a mutex because:
- `lastEventNano` is written on every event (high frequency) and read once per tick (low frequency).
- `resultSeen` and `resultNano` are written at most once.
- No compound read-modify-write is needed.

### 6.2 Event Loop Integration

The `eventLoop()` method is modified to update the activity timestamp on every event:

```go
func (s *ExternalWSServer) eventLoop() {
    for {
        select {
        case <-s.ctx.Done():
            return
        case event, ok := <-s.backend.Events():
            if !ok {
                return
            }
            if event.Type == "" {
                continue
            }

            // Update stall detection timestamps.
            s.lastEventNano.Store(time.Now().UnixNano())
            if event.Type == "result" && !s.resultSeen.Load() {
                s.resultNano.Store(time.Now().UnixNano())
                s.resultSeen.Store(true)
            }

            event = s.normalizeEvent(event)
            s.trackEventMetrics(event)
            _ = s.recordAndBroadcast(event)
        }
    }
}
```

The `lastEventNano` update happens before normalization and broadcast so that stall detection reflects true backend activity, not processing delays.

### 6.3 Stall Monitor Goroutine

```go
const stallTickInterval = 5 * time.Second

func (s *ExternalWSServer) stallMonitor() {
    if s.stallWarningTimeout < 0 && s.stallKillTimeout < 0 && s.resultGracePeriod < 0 {
        return // all disabled
    }

    ticker := time.NewTicker(stallTickInterval)
    defer ticker.Stop()

    warningEmitted := false

    for {
        select {
        case <-s.ctx.Done():
            return
        case <-ticker.C:
            now := time.Now().UnixNano()

            // Phase 0: Result grace period (highest priority — overrides other phases).
            if s.resultGracePeriod > 0 && s.resultSeen.Load() {
                elapsed := time.Duration(now - s.resultNano.Load())
                if elapsed >= s.resultGracePeriod {
                    s.handleStallKill("result_timeout",
                        "agent process did not exit within result grace period")
                    return
                }
                // Once result is seen, only the grace period matters.
                // Skip warning/kill checks — the agent has finished its work.
                continue
            }

            lastEvent := s.lastEventNano.Load()
            if lastEvent == 0 {
                continue // no events yet; agent still starting
            }
            silence := time.Duration(now - lastEvent)

            // Phase 2: Hard kill (checked before warning so we don't warn then immediately kill).
            if s.stallKillTimeout > 0 && silence >= s.stallKillTimeout {
                s.handleStallKill("stall_timeout",
                    fmt.Sprintf("no events for %s, force-killing agent", silence.Truncate(time.Second)))
                return
            }

            // Phase 1: Advisory warning.
            if s.stallWarningTimeout > 0 && !warningEmitted && silence >= s.stallWarningTimeout {
                s.emitStallWarning(silence)
                warningEmitted = true
            }
        }
    }
}
```

### 6.4 Tick Interval

The stall monitor ticks every **5 seconds**. This provides:
- Adequate precision for the 10-second result grace period (worst case: kill fires at ~15s).
- Low overhead for the 10m/50m thresholds.
- Consistency with the PAOP prior art (5s poll interval).

### 6.5 Warning Event

```go
func (s *ExternalWSServer) emitStallWarning(silence time.Duration) {
    _ = s.recordAndBroadcast(Event{
        Type: "system",
        Data: map[string]any{
            "subtype":  "stall_warning",
            "message":  fmt.Sprintf("no events for %s", silence.Truncate(time.Second)),
            "silence":  silence.Seconds(),
            "threshold": s.stallWarningTimeout.Seconds(),
        },
    })
    log.Printf("stall warning: no events for %s", silence.Truncate(time.Second))
}
```

The warning is emitted **once**. It is not repeated — the kill threshold provides the escalation. The `stall_warning` event is a `system` event with a distinctive subtype, consistent with the existing `agent_error` system event pattern (`ws.go:414`).

### 6.6 Kill Sequence

```go
func (s *ExternalWSServer) handleStallKill(reason, message string) {
    // 1. Emit stall_kill system event (best-effort, before killing anything).
    _ = s.recordAndBroadcast(Event{
        Type: "system",
        Data: map[string]any{
            "subtype": "stall_kill",
            "reason":  reason,
            "message": message,
        },
    })
    log.Printf("stall kill (%s): %s", reason, message)

    // 2. Attempt graceful interrupt.
    _ = s.backend.SendInterrupt()

    // 3. Brief grace for interrupt to take effect, then force-kill.
    time.Sleep(2 * time.Second)

    // 4. Synthesize exit event with stall classification.
    exitCode := -1
    _ = s.recordAndBroadcast(Event{
        Type:     "exit",
        ExitCode: &exitCode,
        Data: exitData{
            Code:          -1,
            ErrorDetail:   message,
            ErrorCategory: string(agentErrors.CategoryStall),
            Retryable:     true,
        },
    })

    // 5. Force-kill via Close() which calls backend.Stop()/Close().
    _ = s.Close()
}
```

The kill sequence:
1. Emits a `stall_kill` system event so it appears in the replay buffer and NDJSON log.
2. Sends an interrupt (`SIGINT` equivalent) to give the agent a 2-second window to clean up.
3. Synthesizes an `exit` event with `error_category: "stall"` and `retryable: true` so the daemon's retry logic can act on it.
4. Calls `s.Close()` which cancels the context, closes clients, and calls `backend.Stop()/Close()` (SIGKILL).

The synthesized `exit` event uses exit code `-1` to distinguish stall-kills from normal exits. The `exitLoop()` goroutine will also return when `s.ctx` is cancelled by `s.Close()`, so there is no double-exit-event race — the `exitLoop` checks `s.ctx.Done()` before processing `waitCh`.

### 6.7 Race Between exitLoop and stallMonitor

Both goroutines can trigger session teardown. The race is resolved by context cancellation:

- **Normal exit wins:** `exitLoop` receives from `waitCh`, broadcasts the real exit event, starts the cleanup timer. On the next tick, `stallMonitor` sees `s.ctx.Done()` and returns.
- **Stall kill wins:** `stallMonitor` calls `s.Close()` which cancels `s.ctx`. `exitLoop` sees `s.ctx.Done()` and returns without broadcasting a second exit event.
- **Result grace wins:** Same as stall kill — `handleStallKill` calls `s.Close()`.

No additional locking is needed beyond what `s.Close()` already provides.

### 6.8 Interactive Mode Considerations

In interactive mode (no `AGENT_PROMPT`), the agent may be idle for long periods waiting for user input. This is normal and should not trigger stall detection.

The stall timer resets on **any** event, including `prompt` command processing. However, between prompts there are no events, so the timer would fire.

**Resolution:** The stall timer only starts after the first event is received (`lastEventNano == 0` means "not started yet"). Additionally, `routeCommand()` is modified to reset the activity timestamp when a `prompt` command is received:

```go
func (s *ExternalWSServer) routeCommand(cmd rawCommand) error {
    // Reset stall detection on inbound prompts — the agent is about to work.
    if cmd.Type == "prompt" {
        s.lastEventNano.Store(time.Now().UnixNano())
        // Clear resultSeen for the new turn.
        s.resultSeen.Store(false)
    }
    // ... existing routing logic ...
}
```

This means:
- After a prompt is sent, the 10m/50m timers start fresh.
- Between prompts (idle), `lastEventNano` reflects the last event from the previous turn. If the agent is truly idle (no events after result), the result grace period handles exit. If the result grace period is disabled, the stall timers will eventually fire — but this is correct, because a session sitting idle for 50 minutes after completing work should be reclaimed.

### 6.9 Result Grace and Multi-Turn Sessions

In interactive/multi-turn sessions, a `result` event signals turn completion, not session completion. The agent remains running to accept the next prompt.

When `resultSeen` is set, the grace period timer starts. If a new `prompt` command arrives before the grace period expires, `routeCommand()` clears `resultSeen` (see 6.8), canceling the grace period for the new turn.

If no new prompt arrives within the grace period, the process is killed. This is the desired behavior — a completed turn with no follow-up prompt and no process exit indicates a hung process.

**For sessions that intentionally wait for follow-up prompts**, the daemon should either:
- Set `result_grace_period` to `-1` (disabled) via `AGENT_CONFIG`.
- Send the next prompt before the grace period expires.

### 6.10 Result Grace Period vs Cleanup Timer

These are complementary, not overlapping:

| Timer | Trigger | Default | Purpose |
|-------|---------|---------|---------|
| Result grace period | `result` event seen | 10s | Kill a hung process that finished work but won't exit |
| Cleanup timer | Process exited | 60s | Shut down the sidecar if no client reconnects |

The result grace period fires *before* the process exits. The cleanup timer fires *after* the process exits. They never overlap.

## 7. Error Classification

A new error category is added to `pkg/errors/classify.go`:

```go
const CategoryStall ErrorCategory = "stall"
```

The `Retryable()` method is updated:

```go
func (c ErrorCategory) Retryable() bool {
    switch c {
    case CategoryRateLimit, CategoryDuplicateSession, CategoryUpstreamAPI, CategoryStall:
        return true
    default:
        return false
    }
}
```

Stall errors are retryable because a stalled agent is a transient condition — the same prompt may succeed on retry.

## 8. Constructor Changes

`NewExternalWSServer` is updated to accept stall config:

```go
type StallConfig struct {
    WarningTimeout time.Duration
    KillTimeout    time.Duration
    ResultGrace    time.Duration
}

func NewExternalWSServer(agentType string, backend AgentBackend, stallCfg StallConfig) *ExternalWSServer {
    srv := &ExternalWSServer{
        agentType:           agentType,
        backend:             backend,
        replay:              session.NewReplayBuffer(replayBufferSize),
        clients:             make(map[*wsClient]struct{}),
        cleanupTimeout:      defaultCleanupTimeout,
        stallWarningTimeout: stallCfg.WarningTimeout,
        stallKillTimeout:    stallCfg.KillTimeout,
        resultGracePeriod:   stallCfg.ResultGrace,
    }
    srv.ctx, srv.cancel = context.WithCancel(context.Background())
    return srv
}
```

The `StallConfig` is constructed in `newSidecarFromEnv()` after parsing `AgentConfig`:

```go
func stallConfigFromAgentConfig(cfg AgentConfig) StallConfig {
    return StallConfig{
        WarningTimeout: durationFromSeconds(cfg.StallWarningTimeout, 600),
        KillTimeout:    durationFromSeconds(cfg.StallKillTimeout, 3000),
        ResultGrace:    durationFromSeconds(cfg.ResultGracePeriod, 10),
    }
}

// durationFromSeconds converts integer seconds to time.Duration.
// 0 means "use default". -1 means "disabled" (returns -1ns, which < 0).
func durationFromSeconds(seconds, defaultSeconds int) time.Duration {
    switch {
    case seconds < 0:
        return -1 // disabled; all checks use `> 0` guards
    case seconds == 0:
        return time.Duration(defaultSeconds) * time.Second
    default:
        return time.Duration(seconds) * time.Second
    }
}
```

## 9. Observability

### 9.1 System Events

| Event | Subtype | When | Data Fields |
|-------|---------|------|-------------|
| `system` | `stall_warning` | Silence exceeds warning threshold | `silence` (float64 seconds), `threshold` (float64 seconds), `message` (string) |
| `system` | `stall_kill` | Silence exceeds kill threshold or result grace expires | `reason` ("stall_timeout" or "result_timeout"), `message` (string) |

Both events flow through `recordAndBroadcast`, so they appear in:
- Live WebSocket streams to connected clients.
- The replay buffer (available on reconnect).
- The NDJSON log file (if daemon-side logging is active).

### 9.2 Exit Event

Stall-killed sessions produce an `exit` event with:

```json
{
  "type": "exit",
  "exit_code": -1,
  "data": {
    "code": -1,
    "error_detail": "no events for 50m0s, force-killing agent",
    "error_category": "stall",
    "retryable": true
  }
}
```

### 9.3 Log Output

Standard `log.Printf` calls accompany both warning and kill actions for sidecar-level log aggregation:

```
2026-03-19T12:00:00Z stall warning: no events for 10m0s
2026-03-19T12:40:00Z stall kill (stall_timeout): no events for 50m0s, force-killing agent
```

## 10. Files Changed

| File | Change |
|------|--------|
| `cmd/sidecar/agentconfig.go` | Add `StallWarningTimeout`, `StallKillTimeout`, `ResultGracePeriod` fields to `AgentConfig` |
| `cmd/sidecar/ws.go` | Add atomic stall-tracking fields to `ExternalWSServer`; add `StallConfig` struct; update `NewExternalWSServer` signature; add `stallMonitor()`, `emitStallWarning()`, `handleStallKill()` methods; update `eventLoop()` to store activity timestamp and detect result events; update `routeCommand()` to reset timers on prompt; launch `stallMonitor()` from `ensureStarted()` |
| `cmd/sidecar/main.go` | Add `stallConfigFromAgentConfig()` and `durationFromSeconds()` helpers; thread `StallConfig` into `NewExternalWSServer` call |
| `pkg/errors/classify.go` | Add `CategoryStall`; update `Retryable()` to include it |

## 11. Testing Strategy

### 11.1 Unit Tests (`cmd/sidecar/ws_test.go`)

**Test: stall warning fires after silence threshold**
- Create `ExternalWSServer` with `stallWarningTimeout = 100ms`, backend that emits one event then goes silent.
- Assert a `system` event with `subtype: "stall_warning"` appears in the replay buffer within 200ms.

**Test: stall kill fires after kill threshold**
- Create `ExternalWSServer` with `stallKillTimeout = 200ms`, backend that emits one event then goes silent.
- Assert a `system` event with `subtype: "stall_kill"` appears, followed by an `exit` event with `error_category: "stall"`.
- Assert `backend.Close()` was called.

**Test: result grace period kills hung process**
- Create `ExternalWSServer` with `resultGracePeriod = 100ms`, backend that emits a `result` event but never exits.
- Assert `handleStallKill` fires with reason `"result_timeout"` within 200ms.

**Test: result grace period is cancelled by new prompt**
- Create `ExternalWSServer` with `resultGracePeriod = 200ms`.
- Backend emits a `result` event.
- At 100ms, send a `prompt` command.
- Assert no kill occurs at 300ms.

**Test: normal exit preempts stall detection**
- Create `ExternalWSServer` with `stallKillTimeout = 100ms`.
- Backend exits normally at 50ms.
- Assert no stall_kill event is emitted.

**Test: all phases disabled**
- Create `ExternalWSServer` with all timeouts set to `-1`.
- Backend goes silent.
- Assert no stall events are emitted after 500ms.

**Test: stall detection does not fire before first event**
- Create `ExternalWSServer` with `stallWarningTimeout = 100ms`.
- Backend never emits events (slow startup).
- Assert no stall_warning at 200ms (`lastEventNano` is 0, meaning "not yet started").

### 11.2 Unit Tests (`cmd/sidecar/agentconfig_test.go`)

- Parse `AGENT_CONFIG` with stall fields set → correct values.
- Parse `AGENT_CONFIG` with stall fields omitted → zero values (defaults applied later).
- Parse `AGENT_CONFIG` with negative values → preserved as-is (disable sentinel).

### 11.3 Unit Tests (`pkg/errors/classify_test.go`)

- `CategoryStall.Retryable()` returns `true`.

### 11.4 Integration Tests

- Docker runtime: start a session with a custom `AGENT_CONFIG` that sets short stall timeouts. Verify the container is stopped after the kill threshold.
- Local runtime: same, but verify the sidecar process exits after the kill threshold.

## 12. Rollout

1. **Default-enabled:** Stall detection ships enabled with the PAOP-proven defaults (10m warn, 50m kill, 10s result grace). This matches production experience from the PAOP executor.
2. **Escape hatch:** Operators can disable via `AGENT_CONFIG` with `-1` values if false positives are observed.
3. **Monitoring:** The `stall_warning` event serves as a canary — operators can alert on it before any kill occurs, and adjust thresholds.

## 13. Future Work

- **Daemon-side watchdog:** A backup monitor in `pkg/session` that detects stalled sidecars (not just stalled agents).
- **Repeated warnings:** Emit `stall_warning` at escalating intervals (e.g., 10m, 20m, 30m) instead of once.
- **Per-session override via API:** Allow `POST /sessions` to set stall thresholds without requiring `AGENT_CONFIG` wiring.
- **Metrics export:** Expose stall counts via `/metrics` endpoint for Prometheus scraping.
