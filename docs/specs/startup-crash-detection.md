# Spec: Startup Crash Detection

**Status:** Draft
**Task:** #6
**Effort:** Quick win (~50 LOC)
**Source:** persistence `executor/cli/output_parser.py` `_detect_session_errors()`

## Problem

When an agent crashes on startup (bad config, missing binary, auth failure), the
session ends with an empty or near-empty result and zero token usage. Callers get
a mystery empty response with no indication that the agent never actually ran.

## Solution

Post-session heuristic: if the session produced zero tokens and very little output
(< 2KB), classify it as `startup_crash`. This is a strong signal that the agent
process died before doing any meaningful work.

## Implementation

### In the sidecar normalizer

When emitting the `exit` event at session end, check:

```go
func detectStartupCrash(totalInputTokens, totalOutputTokens int, outputBytes int) bool {
    return totalInputTokens == 0 && totalOutputTokens == 0 && outputBytes < 2048
}
```

### Skip for Codex

Codex reports token usage only in `turn.completed` events, which may not appear
in the accumulator if the agent crashed. For Codex sessions, use a different
heuristic: zero `turn.completed` events + session duration < 10s.

### Exit event enrichment

```json
{
  "type": "exit",
  "data": {
    "exit_code": 1,
    "error_category": "startup_crash",
    "message": "Agent produced no output — likely startup crash"
  }
}
```

### Daemon-side

The session manager should mark the session as `error` with category
`startup_crash` so callers can decide whether to retry with different config.

## Testing

- Agent binary not found → startup_crash detected
- Agent with invalid auth → startup_crash detected (< 2KB error message)
- Normal agent run with small output → NOT classified as crash (has tokens)
- Codex with zero turns + fast exit → startup_crash detected
