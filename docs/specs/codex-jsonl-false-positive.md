# Spec: Codex JSONL False-Positive Prevention

**Status:** Draft
**Task:** #8
**Effort:** Medium (~100 LOC)
**Source:** persistence `executor/cli/output_parser.py`

## Problem

Codex CLI outputs JSONL events that embed raw MCP results, tool outputs, and
API responses inside structured fields. When scanning this output for error
patterns (e.g., "authentication.*error"), the embedded content triggers false
positives. For example, an MCP tool result containing `"authorization": null,
"error": null` matches the auth_error pattern despite being a normal response.

## Solution

When classifying errors from Codex output, extract only the agent's own text —
never scan raw JSONL lines. Specifically:

1. Parse each JSONL line
2. Only extract text from `agent_message` type items and top-level `error` events
3. Ignore all other event types (tool results, MCP content, metadata)
4. Run error pattern classification only against the extracted text

## Implementation

### In `pkg/errors/classify.go`

```go
// ClassifyFromEvents scans only agent-authored text for error patterns.
// Used for Codex output where raw JSONL contains embedded content.
func ClassifyFromEvents(events []NormalizedEvent, tokenCount int) ErrorCategory {
    var agentText strings.Builder
    for _, evt := range events {
        switch evt.Type {
        case "agent_message":
            agentText.WriteString(evt.Data.Text)
            agentText.WriteByte('\n')
        case "error":
            agentText.WriteString(evt.Data.Message)
            agentText.WriteByte('\n')
        }
        // Deliberately skip: tool_use, tool_result, system, progress
    }
    return Classify(agentText.String(), tokenCount)
}
```

### Detection: Is this Codex output?

```go
// First few events contain type indicators:
// Claude: {"type":"system","subtype":"init",...}
// Codex:  {"type":"thread.started",...} or {"type":"session_meta",...}
func isCodexOutput(events []NormalizedEvent) bool {
    for _, evt := range events[:min(5, len(events))] {
        if evt.Type == "thread.started" || evt.Type == "session_meta" {
            return true
        }
    }
    return false
}
```

### Wire into sidecar

The sidecar normalizer already knows which backend it's talking to (Claude vs Codex).
Use the backend type to choose the classification path:

```go
if backend == "codex" {
    category = errors.ClassifyFromEvents(bufferedEvents, totalTokens)
} else {
    category = errors.Classify(rawOutput, totalTokens)
}
```

## Regression Tests

- Codex output with embedded `"authorization": null, "error": null` → NOT auth_error
- Codex output with actual auth failure in agent_message → auth_error
- Claude output with same patterns → classified normally (no special handling needed)
- Mixed MCP content with error-like strings in tool results → ignored
