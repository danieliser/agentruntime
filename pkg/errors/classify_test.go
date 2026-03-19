package errors

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect ErrorCategory
	}{
		// model_not_found
		{"model issue", "There's an issue with the selected model", CategoryModelNotFound},
		{"model not found", "The model does not exist in your account", CategoryModelNotFound},
		{"invalid model", "Error: invalid model specified", CategoryModelNotFound},

		// auth_error
		{"auth failed", "authentication failed: token expired", CategoryAuthError},
		{"api key invalid", "API key is invalid or missing", CategoryAuthError},

		// permission_denied
		{"permission", "Error: permission denied for this resource", CategoryPermissionDenied},

		// rate_limit
		{"rate limit", "Rate limit exceeded, please retry", CategoryRateLimit},

		// duplicate_session
		{"duplicate session", "Error: duplicate session detected", CategoryDuplicateSession},
		{"session exists", "Session already exists with that ID", CategoryDuplicateSession},

		// upstream_api_error
		{"api error code", "API Error: 500 Internal Server Error", CategoryUpstreamAPI},
		{"overloaded", "Service overloaded, try again later", CategoryUpstreamAPI},
		{"503 error", "503 Service Unavailable error", CategoryUpstreamAPI},

		// no match
		{"normal output", "Hello, I'll help you with that task.", CategoryNone},
		{"empty", "", CategoryNone},
		{"code with error word", "if err != nil { return err }", CategoryNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.input)
			if got != tt.expect {
				t.Errorf("Classify(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	retryable := []ErrorCategory{CategoryRateLimit, CategoryDuplicateSession, CategoryUpstreamAPI}
	notRetryable := []ErrorCategory{CategoryNone, CategoryModelNotFound, CategoryAuthError, CategoryPermissionDenied, CategoryStartupCrash}

	for _, c := range retryable {
		if !c.Retryable() {
			t.Errorf("%q should be retryable", c)
		}
	}
	for _, c := range notRetryable {
		if c.Retryable() {
			t.Errorf("%q should not be retryable", c)
		}
	}
}

func TestDetectStartupCrash(t *testing.T) {
	tests := []struct {
		name         string
		input, output, bytes int
		expect       bool
	}{
		{"zero everything", 0, 0, 0, true},
		{"zero tokens small output", 0, 0, 500, true},
		{"zero tokens at threshold", 0, 0, 2047, true},
		{"zero tokens over threshold", 0, 0, 2048, false},
		{"has input tokens", 100, 0, 500, false},
		{"has output tokens", 0, 50, 500, false},
		{"normal session", 1000, 500, 10000, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectStartupCrash(tt.input, tt.output, tt.bytes)
			if got != tt.expect {
				t.Errorf("DetectStartupCrash(%d, %d, %d) = %v, want %v",
					tt.input, tt.output, tt.bytes, got, tt.expect)
			}
		})
	}
}
