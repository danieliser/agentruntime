// Package errors provides error classification for agent session output.
// It scans agent text for known error signatures and categorizes them
// to enable automated retry logic and actionable error reporting.
package errors

import "regexp"

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
)

// Retryable returns true if the error category is potentially recoverable
// with a retry (possibly with backoff or different config).
func (c ErrorCategory) Retryable() bool {
	switch c {
	case CategoryRateLimit, CategoryDuplicateSession, CategoryUpstreamAPI:
		return true
	default:
		return false
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

// DetectStartupCrash returns true if the session metrics indicate the agent
// crashed before doing any meaningful work: zero token usage and very little output.
// Callers should skip this check for Codex sessions where token counts arrive
// via turn.completed events that may not have been received.
func DetectStartupCrash(inputTokens, outputTokens, outputBytes int) bool {
	return inputTokens == 0 && outputTokens == 0 && outputBytes < 2048
}
