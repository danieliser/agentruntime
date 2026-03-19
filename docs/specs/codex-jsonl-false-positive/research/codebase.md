# Codebase Analysis: Codex JSONL False-Positive Prevention

## 1. Executive Summary

The sidecar's error classification system scans accumulated agent text at session exit to categorize errors (auth failures, rate limits, model not found, etc.). The current implementation in `trackEventMetrics` already filters by event type — it only accumulates text from `agent_message` events. However, the Codex backend emits `agent_message` events whose text field may originate from item objects containing embedded tool output references. More critically, the public `Classify()` function in `pkg/errors/classify.go` accepts raw strings with no event-type awareness, making it easy for callers outside the sidecar to scan tool output and trigger false positives. A `ClassifyFromEvents` function would formalize the event-type filtering as a reusable API.

---

## 2. Current Error Classification Pipeline

### 2.1 Pattern Classifier

**File:** `pkg/errors/classify.go:38-63`

```go
var patterns = []pattern{
    {regexp.MustCompile(`(?i)(?:authentication|auth).*(?:failed|error|invalid)`), CategoryAuthError},
    {regexp.MustCompile(`(?i)API key.*(?:invalid|missing|expired)`), CategoryAuthError},
    // ... 12 patterns total
}

func Classify(output string) ErrorCategory {
    for _, p := range patterns {
        if p.re.MatchString(output) {
            return p.category
        }
    }
    return CategoryNone
}
```

`Classify` is a pure string scanner. It has no knowledge of event types, JSONL structure, or whether the text came from an agent message vs. a tool result. Any caller passing concatenated raw output will hit false positives on embedded content.

### 2.2 Sidecar Metrics Accumulator

**File:** `cmd/sidecar/ws.go:307-334`

```go
func (s *ExternalWSServer) trackEventMetrics(event Event) {
    data, _ := event.Data.(map[string]any)
    switch event.Type {
    case "agent_message":
        if text, ok := data["text"].(string); ok {
            s.agentTextBuf.WriteString(text)
            s.agentTextBuf.WriteByte('\n')
            s.totalOutputBytes += len(text)
        }
    case "result":
        // ... accumulate token counts from usage field
    }
}
```

This function runs **after** normalization (`ws.go:300-301`):

```go
event = s.normalizeEvent(event)
s.trackEventMetrics(event)
```

Because normalization strips raw backend data down to `NormalizedAgentMessage` fields, the `text` value tracked here is the agent's own text, not raw JSONL. The sidecar path is already partially protected.

### 2.3 Session Exit Classification

**File:** `cmd/sidecar/ws.go:336-357`

```go
func (s *ExternalWSServer) classifySessionError() agentErrors.ErrorCategory {
    agentText := s.agentTextBuf.String()
    // ... extract metrics ...
    if cat := agentErrors.Classify(agentText); cat != agentErrors.CategoryNone {
        return cat
    }
    if agentErrors.DetectStartupCrash(inputToks, outputToks, outputBytes) {
        return agentErrors.CategoryStartupCrash
    }
    return agentErrors.CategoryNone
}
```

Called once from `exitLoop()` (ws.go:428). The accumulated `agentTextBuf` is passed to `Classify()`.

---

## 3. Codex Event Emission — Where Embedded Content Enters

### 3.1 Exec JSONL Mode (Prompt Mode)

**File:** `cmd/sidecar/codex.go:244-296`

```go
func (b *codexBackend) readExecJSONL() {
    // ...
    case strings.HasPrefix(eventType, "item.completed"):
        item, _ := raw["item"].(map[string]any)
        itemType, _ := item["type"].(string)
        switch itemType {
        case "agent_message":
            text, _ := item["text"].(string)
            b.emit(Event{Type: "agent_message", Data: map[string]any{
                "text": text, "final": true, "item": item,
            }})
        default:
            b.emit(Event{Type: "tool_result", Data: raw})
        }
```

For `agent_message` items, the `text` field is extracted from `item["text"]`. The full `item` object (which may contain nested content arrays with tool references) is also passed as `"item"` in the data map, but normalization strips it.

For non-agent-message items, the event is emitted as `tool_result` — `trackEventMetrics` correctly ignores these.

### 3.2 Interactive Mode (App-Server JSON-RPC)

**File:** `cmd/sidecar/codex.go:717-792`

```go
case "item/agentMessage/delta":
    eventData := cloneMap(params)
    eventData["text"] = text
    eventData["final"] = false
    b.emit(Event{Type: "agent_message", Data: eventData})

case "item/completed":
    // For agent_message type:
    eventData := cloneMap(params)
    eventData["text"] = text
    eventData["final"] = true
    b.emit(Event{Type: "agent_message", Data: eventData})
```

The `cloneMap(params)` call includes ALL notification parameters from the JSON-RPC message. This can include nested `item` objects with `content` arrays containing tool output text. However, `normalizeCodexAgentMessage()` strips this down to just `NormalizedAgentMessage` fields before `trackEventMetrics` runs.

### 3.3 Normalization — The Filtering Step

**File:** `cmd/sidecar/normalize.go:106-128`

```go
func normalizeCodexAgentMessage(raw map[string]any) map[string]any {
    msg := NormalizedAgentMessage{
        TurnID: stringVal(raw, "turnId"),
        ItemID: stringVal(raw, "itemId"),
    }
    if final, ok := raw["final"].(bool); ok && final {
        msg.Text = stringVal(raw, "text")
        if item, ok := raw["item"].(map[string]any); ok {
            msg.Text = stringVal(item, "text")  // Prefers item.text
        }
    } else {
        msg.Delta = true
        msg.Text = stringVal(raw, "delta")
        if msg.Text == "" {
            msg.Text = stringVal(raw, "text")
        }
    }
    return structToMap(msg)
}
```

After normalization, only `text`, `delta`, `model`, `usage`, `turn_id`, `item_id` survive in the map. The raw `item` object and its nested content are discarded. This is the key protection in the sidecar path.

---

## 4. The False-Positive Scenarios

### Scenario A: Direct `Classify()` on Raw JSONL (The Main Risk)

If any caller outside the sidecar (daemon, SDK consumer, log analyzer) concatenates raw JSONL lines and calls `Classify()`, embedded content triggers false positives:

```json
{"type":"item.completed","item":{"type":"command_execution","aggregatedOutput":"Error: authentication failed for user admin"}}
```

The string `"authentication failed"` matches `CategoryAuthError` even though it's a tool output, not an agent error.

**Real-world trigger:** Codex MCP tool results can contain JSON with fields like:
```json
{"authorization": null, "error": null}
```
The concatenation `authorization...error` matches the auth_error pattern `(?i)(?:authentication|auth).*(?:failed|error|invalid)` when "auth" and "error" appear in the same scanned text window.

### Scenario B: Agent Quoting Tool Output (Subtle Risk)

Even with event-type filtering, the agent itself may quote tool output in its message text:

> I ran the command and got: `authentication error: invalid credentials`

This is the agent's own text, so event-type filtering won't help. However, this is arguably a correct classification — the agent is reporting an auth error it encountered. The existing spec (Task #8) considers this acceptable.

### Scenario C: Codex Item Text vs. Content (Edge Case)

Codex `item.completed` events for `agent_message` type have a `text` field that is the agent's synthesized message. But some Codex versions may embed tool output summaries in this text field. The normalization step extracts `item["text"]` (line 117 of normalize.go), which is the agent's text, not tool content. This is currently safe.

---

## 5. What `trackEventMetrics` Catches vs. Misses

| Event Type | Tracked? | Content Scanned |
|---|---|---|
| `agent_message` | Yes | `.text` after normalization (agent's own text) |
| `tool_use` | No | Skipped entirely |
| `tool_result` | No | Skipped entirely |
| `result` | Partial | Only token counts from `.usage`, not text |
| `system` | No | Skipped entirely |
| `error` | No | **Missed** — sidecar-level error messages not scanned |
| `exit` | No | Generated by sidecar itself, not backend |

**Gap:** Top-level `error` events from the backend (e.g., `{"type":"error","data":{"message":"API Error: 503"}}`) are not accumulated into `agentTextBuf`. These events can carry meaningful error information that the classifier should see.

---

## 6. Existing Tests

**File:** `pkg/errors/classify_test.go`

- `TestClassify`: 12 positive cases + 3 negative cases for pattern matching
- `TestRetryable`: Verifies retryability for all categories
- `TestDetectStartupCrash`: 7 cases covering threshold boundaries

**Missing tests:**
- No test for false positives from embedded tool content
- No test for event-type filtering behavior
- No integration test combining Codex JSONL parsing + classification

---

## 7. Existing Spec Gaps

**File:** `docs/specs/codex-jsonl-false-positive.md`

The existing draft spec proposes `ClassifyFromEvents(events []NormalizedEvent, tokenCount int)` but has several issues relative to the current codebase:

1. **`NormalizedEvent` type doesn't exist** — Events in the sidecar use `Event` (ws.go:31-37) with `Data any`. There's no unified event struct with typed data fields.

2. **Signature mismatch** — `Classify()` takes a single string and returns `ErrorCategory`. The proposed `ClassifyFromEvents` takes a `tokenCount` parameter that `Classify` doesn't accept (it was removed or never added — the spec references an older signature).

3. **`isCodexOutput()` is unnecessary** — The sidecar already knows the backend type (`s.agentType`). The detection function is only needed for contexts that receive a raw event stream without metadata.

4. **The sidecar already filters** — `trackEventMetrics` already only accumulates `agent_message` text. The proposed function duplicates this filtering for external callers, which is the real value.

---

## 8. Proposed `ClassifyFromEvents` Design

The function should live in `pkg/errors/classify.go` and accept a minimal event representation:

```go
type EventForClassification struct {
    Type string
    Text string
}

func ClassifyFromEvents(events []EventForClassification) ErrorCategory {
    var buf strings.Builder
    for _, evt := range events {
        switch evt.Type {
        case "agent_message", "error":
            buf.WriteString(evt.Text)
            buf.WriteByte('\n')
        }
    }
    return Classify(buf.String())
}
```

Key design decisions:
- **No `tokenCount` parameter** — `Classify()` doesn't use it; `DetectStartupCrash` takes separate int args
- **Scans `error` events too** — unlike current `trackEventMetrics`, which misses them
- **Simple event struct** — avoids importing sidecar types into `pkg/errors`
- **Delegates to existing `Classify()`** — single source of truth for patterns

### Wire-In Points

**Sidecar (ws.go):** Replace `classifySessionError`'s direct `Classify(agentText)` call with `ClassifyFromEvents` fed by accumulated events. Alternatively, also accumulate `error` event text in `trackEventMetrics` and keep using `Classify` — simpler change, same effect.

**Daemon (pkg/api/):** When the daemon replays NDJSON logs for post-hoc classification, it should use `ClassifyFromEvents` instead of scanning raw log text.

---

## 9. Files to Modify

| File | Change |
|---|---|
| `pkg/errors/classify.go` | Add `EventForClassification` struct and `ClassifyFromEvents` function |
| `pkg/errors/classify_test.go` | Add false-positive regression tests, `ClassifyFromEvents` unit tests |
| `cmd/sidecar/ws.go` | Optionally accumulate `error` event text in `trackEventMetrics`; or switch `classifySessionError` to use `ClassifyFromEvents` |

---

## 10. Call Graph

```
codexBackend.readExecJSONL()       codexBackend.handleNotification()
    │ emit agent_message                │ emit agent_message
    ▼                                   ▼
ExternalWSServer.eventLoop()
    │
    ├── normalizeEvent()  ← strips to NormalizedAgentMessage
    │       └── normalizeCodexAgentMessage()
    │
    ├── trackEventMetrics()  ← accumulates .text from agent_message only
    │       └── agentTextBuf.WriteString(text)
    │
    └── recordAndBroadcast()  ← sends to clients + replay buffer
            │
            ▼
exitLoop()
    └── classifySessionError()
            ├── Classify(agentTextBuf.String())  ← pattern scan
            └── DetectStartupCrash(...)          ← heuristic fallback
```
