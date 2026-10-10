package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeSyncRejectsUnusableOAuth(t *testing.T) {
	for _, source := range []string{"fresh", "stale", "extracted"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			raw := `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`
			mock := &mockExtractor{result: raw}
			if source != "extracted" {
				path := filepath.Join(dir, credentialsSubdir, claudeCacheFile)
				os.MkdirAll(filepath.Dir(path), 0700)
				os.WriteFile(path, []byte(raw), 0600)
				if source == "stale" {
					old := time.Now().Add(-time.Hour)
					os.Chtimes(path, old, old)
					mock.err = errors.New("private-extractor-error")
				}
			}
			path, err := newSyncWithExtractor(dir, mock).ClaudeCredentialsFile()
			if err == nil || path != "" {
				t.Fatal("unusable OAuth was accepted")
			}
			if err.Error() != "Claude authentication unavailable: invalid credentials" {
				t.Fatalf("unsanitized auth error: %v", err)
			}
		})
	}
}

func TestClaudeCredentialValidationFormats(t *testing.T) {
	tests := []struct {
		name, raw string
		valid     bool
	}{
		{"malformed", `{"private-secret":`, false},
		{"null", `null`, false},
		{"null-oauth", `{"claudeAiOauth":null}`, false},
		{"empty-oauth", `{"claudeAiOauth":{}}`, false},
		{"wrong-token-type", `{"claudeAiOauth":{"accessToken":123}}`, false},
		{"expired-no-refresh", `{"claudeAiOauth":{"accessToken":"private-secret","expiresAt":1}}`, false},
		{"access-no-expiry", `{"claudeAiOauth":{"accessToken":"test-access"}}`, true},
		{"future-access", `{"claudeAiOauth":{"accessToken":"test-access","expiresAt":9999999999999}}`, true},
		{"expired-refreshable", `{"claudeAiOauth":{"accessToken":"test-access","refreshToken":"test-refresh","expiresAt":1}}`, true},
		{"refresh-only", `{"claudeAiOauth":{"refreshToken":"test-refresh","expiresAt":0}}`, true},
		{"explicit-token-format", `{"token":"test-token"}`, true},
		{"explicit-api-key-format", `{"apiKey":"test-api-key"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateClaudeCredentials([]byte(tt.raw))
			if (err == nil) != tt.valid {
				t.Fatalf("expected valid=%v, got error %v", tt.valid, err)
			}
			if err != nil && !errors.Is(err, ErrClaudeAuthUnavailable) {
				t.Fatal("missing stable auth error")
			}
		})
	}
}

func TestClaudeSyncRejectsMalformedExtractionWithoutPublishingCache(t *testing.T) {
	dir := t.TempDir()
	_, err := newSyncWithExtractor(dir, &mockExtractor{result: `{"secret":`}).ClaudeCredentialsFile()
	if !errors.Is(err, ErrClaudeAuthUnavailable) {
		t.Fatalf("expected sanitized auth error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, credentialsSubdir, claudeCacheFile)); !os.IsNotExist(err) {
		t.Fatal("invalid extraction published into cache")
	}
}
