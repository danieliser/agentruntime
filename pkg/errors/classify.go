// Package errors provides error classification for agent session output.
// It scans agent text for known error signatures and categorizes them
// to enable automated retry logic and actionable error reporting.
package errors

import (
	"regexp"
	"strings"
)

// ErrorCategory identifies the type of agent session error.
type ErrorCategory string

const (
	CategoryNone             ErrorCategory = ""
	CategoryModelNotFound    ErrorCategory = "model_not_found"
	CategoryAuthError        ErrorCategory = "auth_error"
	CategoryPermissionDenied ErrorCategory = "permission_denied"
	CategoryRateLimit        ErrorCategory = "rate_limit"
	CategoryDuplicateSession ErrorCategory = "duplicate_session"
	CategoryUpstreamAPI      ErrorCategory = "upstream_api_error"
	CategoryStartupCrash     ErrorCategory = "startup_crash"
	CategoryStall            ErrorCategory = "stall"
)

// Retryable returns true if the error category is potentially recoverable
// with a retry (possibly with backoff or different config).
func (c ErrorCategory) Retryable() bool {
	switch c {
	case CategoryRateLimit, CategoryDuplicateSession, CategoryUpstreamAPI, CategoryStall:
		return true
	default:
		return false
	}
}

// Fatal returns true if the error is unrecoverable within the current session
// and the agent should be killed immediately to avoid wasting resources.
func (c ErrorCategory) Fatal() bool {
	switch c {
	case CategoryAuthError, CategoryRateLimit, CategoryModelNotFound:
		return true
	default:
		return false
	}
}

// ActionMessage returns a human-readable remediation hint for the error.
func (c ErrorCategory) ActionMessage() string {
	switch c {
	case CategoryAuthError:
		return "Authentication failed. Run 'claude setup-token' (Claude) or 'codex login' (Codex) on the host to refresh credentials."
	case CategoryRateLimit:
		return "API usage limit reached. Check your plan limits or wait for the reset window."
	case CategoryModelNotFound:
		return "The requested model is not available. Check model name and account access."
	default:
		return ""
	}
}

type pattern struct {
	re       *regexp.Regexp
	category ErrorCategory
}

// Order matters — first match wins. More specific patterns before broader ones.
var patterns = []pattern{
	{regexp.MustCompile(`(?i)There's an issue with the selected model`), CategoryModelNotFound},
	{regexp.MustCompile(`(?i)model.*(?:not exist|not found|not available|no access)`), CategoryModelNotFound},
	{regexp.MustCompile(`(?i)invalid.*model`), CategoryModelNotFound},
	{regexp.MustCompile(`(?i)(?:authentication|auth).*(?:failed|error|invalid)`), CategoryAuthError},
	{regexp.MustCompile(`(?i)API key.*(?:invalid|missing|expired)`), CategoryAuthError},
	{regexp.MustCompile(`(?i)refresh.token.*(?:reused|expired|invalid|already used)`), CategoryAuthError},
	{regexp.MustCompile(`(?i)Failed to refresh token`), CategoryAuthError},
	{regexp.MustCompile(`(?i)access token could not be refreshed`), CategoryAuthError},
	{regexp.MustCompile(`(?i)You have reached your.*API usage limits`), CategoryRateLimit},
	{regexp.MustCompile(`(?i)permission.*denied`), CategoryPermissionDenied},
	{regexp.MustCompile(`(?i)rate.*limit.*exceeded`), CategoryRateLimit},
	{regexp.MustCompile(`(?i)duplicate session`), CategoryDuplicateSession},
	{regexp.MustCompile(`(?i)session.*already exists`), CategoryDuplicateSession},
	{regexp.MustCompile(`API Error:\s*\d{3}`), CategoryUpstreamAPI},
	{regexp.MustCompile(`"type"\s*:\s*"error".*"Internal server error"`), CategoryUpstreamAPI},
	{regexp.MustCompile(`(?i)(?:overloaded|529|503).*(?:error|try again)`), CategoryUpstreamAPI},
}

// Classify scans output text for known error patterns and returns the
// matching category. Returns CategoryNone if no pattern matches.
func Classify(output string) ErrorCategory {
	for _, p := range patterns {
		if p.re.MatchString(output) {
			return p.category
		}
	}
	return CategoryNone
}

// EventForClassification represents a minimal event for error classification.
// It includes only the fields needed to extract agent-authored text.
type EventForClassification struct {
	Type string
	Text string
}

// ClassifyFromEvents scans only agent-authored text from a sequence of events,
// ignoring embedded tool output and API responses. This prevents false positives
// from Codex JSONL that embeds MCP results and raw API responses in event data.
//
// Only agent_message and error events are scanned; tool results, system events,
// and other metadata are ignored.
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

// DetectStartupCrash returns true if the session metrics indicate the agent
// crashed before doing any meaningful work: zero token usage and very little output.
// Callers should skip this check for Codex sessions where token counts arrive
// via turn.completed events that may not have been received.
func DetectStartupCrash(inputTokens, outputTokens, outputBytes int) bool {
	return inputTokens == 0 && outputTokens == 0 && outputBytes < 2048
}
