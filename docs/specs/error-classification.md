# Spec: Error Pattern Classification System

**Status:** Draft
**Task:** #3
**Effort:** Quick win (single file, ~150 LOC)
**Source:** persistence `executor/cli/output_parser.py`

## Problem

Agentruntime's sidecar normalizer sets `IsError: true/false` on events but provides
no categorization. Callers can't distinguish a rate limit from a model not found from
an auth failure — they all look the same. This makes automated retry logic impossible
and debugging painful.

## Solution

Add an `ErrorCategory` field to normalized error events and a regex-based classifier
that scans agent output for known error signatures.

## Error Categories

| Category | Trigger Pattern | Retryable |
|---|---|---|
| `model_not_found` | "issue with the selected model", "model not exist/found/available" | No |
| `auth_error` | "authentication failed", "API key invalid/missing/expired" | No (needs human) |
| `permission_denied` | "permission denied" | No |
| `rate_limit` | "rate limit exceeded" | Yes (backoff) |
| `duplicate_session` | "duplicate session", "session already exists" | Yes (new session ID) |
| `upstream_api_error` | "API Error: \d{3}", "overloaded", "529", "503" | Yes (backoff) |
| `startup_crash` | Zero tokens + output < 2KB | Depends |

## Implementation

### File: `pkg/errors/classify.go` (new)

```go
package errors

type ErrorCategory string

const (
    CategoryModelNotFound   ErrorCategory = "model_not_found"
    CategoryAuthError       ErrorCategory = "auth_error"
    CategoryPermissionDenied ErrorCategory = "permission_denied"
    CategoryRateLimit       ErrorCategory = "rate_limit"
    CategoryDuplicateSession ErrorCategory = "duplicate_session"
    CategoryUpstreamAPI     ErrorCategory = "upstream_api_error"
    CategoryStartupCrash    ErrorCategory = "startup_crash"
    CategoryUnknown         ErrorCategory = "unknown"
)

func (c ErrorCategory) Retryable() bool { ... }

func Classify(output string, tokenCount int) ErrorCategory { ... }
```

### Wire into sidecar normalizer

In `cmd/sidecar/normalize.go`, when emitting error events:
```go
event.ErrorCategory = errors.Classify(event.Data.Text, totalTokens)
```

### Wire into daemon session handling

In `pkg/session/`, when a session ends with an error, classify and store
the category for the caller to inspect.

### Normalized event change

Add `error_category` field to the error event JSON:
```json
{
  "type": "error",
  "data": {
    "message": "...",
    "error_category": "rate_limit",
    "retryable": true
  }
}
```

## Codex JSONL Caveat

Codex JSONL embeds MCP results and tool outputs that contain strings like
"authorization" + "error":null which false-positive on auth_error patterns.
The classifier must only scan `agent_message` text and top-level `error`
events — never raw JSONL content. See Task #8 for the full handling.

## Testing

- Unit tests for each pattern with positive and negative cases
- Codex JSONL false-positive regression test
- Integration test: spawn agent with invalid model → verify `model_not_found`
