# Codebase Analysis: Two-Phase Stall Detection

## 1. Executive Summary

The sidecar currently has **zero event-stream activity monitoring**. Process termination relies entirely on Go context cancellation (signal handling) and the post-exit cleanup timer. There is no mechanism to detect a stalled agent, warn operators, or force-kill a hung process. All the infrastructure for adding stall detection exists — the event loop, process kill chain, and config channel are well-structured — but the actual monitoring goroutine and timeout logic are missing.

---

## 2. Event Loop — The Intercept Point

**File:** `cmd/sidecar/ws.go:288-305`

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
            event = s.normalizeEvent(event)
            s.trackEventMetrics(event)
            _ = s.recordAndBroadcast(event)
        }
    }
}
```

**Analysis:** Every event from the backend flows through `eventLoop()`. This is the natural place to update a `lastEventTime` timestamp. The existing `trackEventMetrics()` call (line 301) already classifies events for error detection — stall detection can follow the same pattern: observe every event, update a shared timestamp.

**Key observation:** The event channel (`backend.Events()`) is buffered at 64 (`claude.go:153`, `codex.go:131`, `generic.go:43`). A stalled agent produces no events at all — the channel blocks indefinitely. The stall detector goroutine must run independently of the event loop, using wall-clock time comparison against the last activity timestamp.

---

## 3. Process Exit — Current Mechanism

**File:** `cmd/sidecar/ws.go:398-440`

```go
func (s *ExternalWSServer) exitLoop() {
    waitCh := s.backend.Wait()
    select {
    case <-s.ctx.Done():
        return
    case result, ok := <-waitCh:
        // ... classify error, broadcast exit event, start cleanup timer
    }
}
```

**Analysis:** The exit loop blocks on `backend.Wait()` — a channel that receives exactly once when the process terminates. There is no timeout. If the process hangs (e.g., MCP server prevents clean exit after result), this goroutine blocks forever.

**Result-marker early exit** should watch for `result` events in the event loop. When a `result` event is seen, a grace timer starts. If the process hasn't exited within the grace period, the stall detector force-kills it.

---

## 4. Result Event Detection

### Claude Backend
**File:** `cmd/sidecar/claude.go:479-510`

Claude emits a `result` event via `handleResult()` when it receives a `{"type":"result",...}` line on stdout. This maps to the `result` event type in the normalized stream.

### Codex Backend (Interactive)
**File:** `cmd/sidecar/codex.go:717-741`

Codex emits a `result` event when it receives a `turn/completed` JSON-RPC notification. The event type is `"result"` after mapping.

### Codex Backend (Prompt Mode)
**File:** `cmd/sidecar/codex.go:244-296`

Codex exec mode emits `result` when it sees `turn.completed` in the JSONL stream (line 283-284).

### Normalized Event Types
**File:** `cmd/sidecar/normalize.go`

All backends normalize to the same event types:
- `agent_message` — text output (streaming deltas or final)
- `tool_use` — tool call start
- `tool_result` — tool call completion
- `result` — **turn/session completion marker**
- `progress` — status updates
- `system` — system events (stderr, hooks)
- `error` — error events
- `exit` — process termination (added by exitLoop, not backend)

**The `result` event is the universal completion marker.** When seen, the agent has finished its work. Process exit should follow shortly. If it doesn't, the process is hung.

---

## 5. Process Kill Chain

### ExternalWSServer.Close()
**File:** `cmd/sidecar/ws.go:185-202`

```go
func (s *ExternalWSServer) Close() error {
    s.cancel()           // cancel context
    s.stopCleanupTimer()
    s.replay.Close()
    // ... close all WS clients
    // ... call backend.Close() or backend.Stop()
}
```

### Claude Backend Kill
**File:** `cmd/sidecar/claude.go:328-354`

```go
func (b *ClaudeBackend) Stop() error {
    b.running = false
    process.Kill()  // os.Process.Kill() — sends SIGKILL
    server.Stop()   // stop MCP server
    close(b.done)
}
```

### Codex Backend Kill
**File:** `cmd/sidecar/codex.go:458-485`

```go
func (b *codexBackend) Close() error {
    cancel()    // cancel context
    stdin.Close()
    closeFn()   // cmd.Process.Kill()
    close(b.done)
    close(b.events)
}
```

### Generic Backend Kill
**File:** `cmd/sidecar/generic.go:164-188`

```go
func (b *genericCommandBackend) Close() error {
    process.Process.Kill()
    close(b.waitCh)
    close(b.events)
    close(b.done)
}
```

**Summary:** All backends support `.Close()` or `.Stop()` which sends SIGKILL to the agent process. The stall detector can call `s.backend` close methods directly, or more cleanly, cancel the server context via `s.cancel()` followed by `s.Close()`.

**Recommended kill sequence for stall detection:**
1. Send interrupt first (`s.backend.SendInterrupt()`) — gives the agent a chance to clean up
2. Wait brief grace (2-3s)
3. Force kill via `s.Close()` which triggers `backend.Stop()/Close()`

---

## 6. AGENT_CONFIG — The Configuration Channel

**File:** `cmd/sidecar/agentconfig.go`

```go
type AgentConfig struct {
    Model         string            `json:"model,omitempty"`
    ResumeSession string            `json:"resume_session,omitempty"`
    Env           map[string]string `json:"env,omitempty"`
    ApprovalMode  string            `json:"approval_mode,omitempty"`
    MaxTurns      int               `json:"max_turns,omitempty"`
    AllowedTools  []string          `json:"allowed_tools,omitempty"`
    Effort        string            `json:"effort,omitempty"`
}
```

**How it flows:**
1. Daemon serializes `AgentConfig` as JSON into `AGENT_CONFIG` env var
2. `cmd/sidecar/main.go:107` calls `parseAgentConfig()` at startup
3. Fields are threaded into the backend constructor (`main.go:280-306`)

**Stall detection config should be added here:**
```go
type AgentConfig struct {
    // ... existing fields ...

    // StallWarningTimeout is the duration of event-stream silence before
    // emitting an advisory warning event. Default: 10m. 0 = disabled.
    StallWarningTimeout Duration `json:"stall_warning_timeout,omitempty"`

    // StallKillTimeout is the duration of event-stream silence before
    // force-killing the agent process. Default: 50m. 0 = disabled.
    StallKillTimeout Duration `json:"stall_kill_timeout,omitempty"`

    // ResultGracePeriod is how long to wait after a result event for the
    // process to exit before force-killing. Default: 10s. 0 = disabled.
    ResultGracePeriod Duration `json:"result_grace_period,omitempty"`
}
```

**Note:** `AgentConfig` currently uses plain `int` for `MaxTurns` and `string` for durations elsewhere. The spec should decide whether to use seconds (int) for simplicity or Go duration strings for flexibility. Seconds are simpler and match SIDECAR_CLEANUP_TIMEOUT precedent (`main.go:169-189`).

---

## 7. Where the Stall Monitor Goroutine Should Live

### Option A: Inside ExternalWSServer (Recommended)

The `ExternalWSServer` already owns:
- The event loop (activity source)
- The exit loop (process lifecycle)
- The cleanup timer (post-exit lifecycle)
- The backend reference (for kill)
- The context (for cancellation)

Adding a `stallMonitor()` goroutine launched from `ensureStarted()` alongside `eventLoop()` and `exitLoop()` is the cleanest approach. It shares the same lifecycle.

```go
func (s *ExternalWSServer) ensureStarted() error {
    s.startOnce.Do(func() {
        // ...
        go s.eventLoop()
        go s.exitLoop()
        go s.stallMonitor()  // NEW
    })
    return s.getStartErr()
}
```

### Option B: Inside Each Backend

Each backend could have its own stall monitor. **Not recommended** — it duplicates logic across Claude, Codex, and Generic backends, and the kill decision belongs at the server level, not the backend level.

### Option C: Daemon-Side (pkg/session)

The daemon already tracks `LastActivity` on sessions (`pkg/session/session.go:42`). A daemon-side monitor could periodically check all sessions and kill stalled ones. **Downside:** requires daemon↔sidecar kill signaling via WS, doesn't work for standalone sidecar usage, and adds latency to the kill decision.

**Verdict: Option A.** The stall monitor belongs in `ExternalWSServer`, launched alongside the existing goroutines.

---

## 8. Existing Activity Tracking (Daemon Side)

**File:** `pkg/session/session.go:42,102-108`

```go
type Session struct {
    LastActivity  *time.Time `json:"last_activity,omitempty"`
    // ...
}

func (s *Session) RecordActivity() {
    now := time.Now()
    s.LastActivity = &now
}
```

**File:** `pkg/api/sessionio.go:116-155`

```go
func parseAndTrackEvent(sess *session.Session, line []byte) {
    // Any event triggers activity.
    sess.RecordActivity()
    // ...
}
```

The daemon already records per-event activity timestamps. This is useful for external monitoring but doesn't help the sidecar — the sidecar needs its own activity tracking to make autonomous kill decisions.

---

## 9. Sidecar Shutdown Flow

**File:** `cmd/sidecar/main.go:36-90`

```go
func run() error {
    // ... create server, HTTP server, set shutdown func
    select {
    case err := <-errCh:       // HTTP server error
    case <-cleanupCh:          // cleanup timer elapsed (agent exited + no clients)
    case <-signalCtx.Done():   // SIGINT/SIGTERM
    }
}
```

The sidecar exits when:
1. HTTP server fails
2. Cleanup timer fires (agent exited, no WS clients reconnected within timeout)
3. Signal received (SIGINT/SIGTERM)

**The stall detector's kill action should emit an exit event through normal channels**, then let the existing cleanup timer handle sidecar shutdown. This avoids adding a fourth exit path.

---

## 10. Event Types That Count as Activity

All event types should reset the stall timer **except**:
- Events with empty Type (already filtered at `ws.go:298`)

Even `system` events (stderr, hooks) indicate the agent is doing something. The stall detector cares about "is the process producing any output at all," not "is it producing useful output."

---

## 11. Error Classification Integration

**File:** `pkg/errors/classify.go`

The existing error classification system recognizes categories like `auth_error`, `rate_limit`, `startup_crash`, etc. A new `CategoryStall` should be added:

```go
const CategoryStall ErrorCategory = "stall"
```

With `Retryable() = true` — a stalled agent can potentially succeed on retry.

The stall detector should set this category on the exit event it generates, so daemon-side retry logic can act on it.

---

## 12. Metrics Already Tracked in ExternalWSServer

**File:** `cmd/sidecar/ws.go:94-99`

```go
type ExternalWSServer struct {
    metricsMu       sync.Mutex
    totalInputToks  int
    totalOutputToks int
    totalOutputBytes int
    agentTextBuf    strings.Builder
}
```

These metrics are accumulated by `trackEventMetrics()` and consumed by `classifySessionError()` at exit. The stall detection fields (`lastEventTime`, `resultSeen`, `resultSeenAt`) follow the same pattern — protected by the same or a new mutex, updated in the event loop, consumed by the stall monitor goroutine.

---

## 13. Concurrency Model

The stall monitor needs to read `lastEventTime` set by the event loop. Three approaches:

1. **Atomic time** (`atomic.Int64` storing Unix nanos) — lock-free, lowest overhead
2. **Shared mutex** — consistent with existing `metricsMu` pattern
3. **Channel-based** — event loop sends ticks to stall monitor

**Recommendation:** `atomic.Int64` for `lastEventTime` (read-heavy by the ticker, written once per event). Use a separate `sync.Mutex` for `resultSeen`/`resultSeenAt` since those trigger state transitions.

---

## 14. Legacy V1 Sidecar

**File:** `cmd/sidecar/legacy_v1.go`

The legacy PTY sidecar has no structured events — raw byte streams only. Stall detection should **not** be added to the legacy path. It's a fallback for old env var configurations and will eventually be removed.

---

## 15. PAOP Prior Art

**File:** `docs/PERSISTENCE-KNOWLEDGE-EXTRACTION.md` (section 6)

The PAOP executor already implements two-phase stall detection:
- **Phase 1:** Advisory warning at `stall_timeout` (default 600s = 10 min). Logs only.
- **Phase 2:** Hard kill at `5x stall_timeout` (default 3000s = 50 min). Raises `AgentStallError`.
- **Result marker detection:** Scans last 32KB of log for `"type":"result"` or `"type":"turn.completed"`. If found, 2 poll cycle grace period then exit.

The agentruntime implementation should match these semantics but with direct in-process event observation instead of log file scanning.

---

## 16. Proposed New/Modified Files

| File | Change |
|------|--------|
| `cmd/sidecar/agentconfig.go` | Add `StallWarningTimeout`, `StallKillTimeout`, `ResultGracePeriod` fields |
| `cmd/sidecar/ws.go` | Add `stallMonitor()` goroutine, `lastEventTime` atomic, result-seen tracking; launch from `ensureStarted()` |
| `cmd/sidecar/ws.go` | Emit `system` event with `subtype: "stall_warning"` at warning threshold |
| `cmd/sidecar/ws.go` | Emit `system` event with `subtype: "stall_kill"` then force-kill at hard threshold |
| `cmd/sidecar/ws.go` | Result-grace-period timer: start on `result` event, kill after expiry if process alive |
| `pkg/errors/classify.go` | Add `CategoryStall ErrorCategory = "stall"` |
| `cmd/sidecar/ws_test.go` | Tests for stall warning, stall kill, result grace period, config override |
| `cmd/sidecar/agentconfig_test.go` | Tests for parsing stall detection config fields |

---

## 17. Data Flow Diagram

```
AGENT_CONFIG env var (daemon → sidecar)
    ↓
parseAgentConfig() → AgentConfig{StallWarningTimeout, StallKillTimeout, ResultGracePeriod}
    ↓
newBackend() → backend (unchanged)
    ↓
NewExternalWSServer() → receives stall config
    ↓
ensureStarted()
    ├── go eventLoop()     ← updates lastEventTime on every event
    │                      ← sets resultSeen=true on "result" events
    ├── go exitLoop()      ← unchanged, handles normal exit
    └── go stallMonitor()  ← NEW: periodic check against lastEventTime
         ├── if now - lastEventTime > warningTimeout → emit stall_warning system event
         ├── if now - lastEventTime > killTimeout    → emit stall_kill, force-kill backend
         └── if resultSeen && now - resultSeenAt > gracePeriod → force-kill backend
```

---

## 18. Open Questions for Spec

1. **Should stall detection be disabled by default?** The PAOP defaults (10m warn, 50m kill) are battle-tested. Recommendation: enabled by default with those values.

2. **Should the warning event be repeated?** E.g., every 5 minutes after the first warning? Or once only? PAOP logs on every poll cycle (5s). Recommendation: emit once, then again at 2x, 3x, etc. of the warning interval.

3. **Should the daemon also monitor?** The daemon has `LastActivity` on sessions. A daemon-side backup monitor could catch cases where the sidecar itself hangs (not just the agent). Recommendation: out of scope for this spec; daemon-side monitoring is a separate feature.

4. **Interactive vs. prompt mode:** In interactive mode, long silences between user prompts are normal. Should stall detection only activate after a prompt is sent? Or always? Recommendation: always active but with a longer default for interactive sessions (or disabled). The daemon can configure per-session via AGENT_CONFIG.

5. **Timer resolution:** How often should the stall monitor tick? PAOP uses 5s poll intervals. Recommendation: 30s tick interval is sufficient — we don't need sub-minute precision for 10m/50m thresholds.
