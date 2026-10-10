package credentials

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// ErrClaudeAuthUnavailable contains no source paths, credential values, or
// platform extractor errors and can safely cross the dispatch boundary.
var ErrClaudeAuthUnavailable = errors.New("Claude authentication unavailable: invalid credentials")

// ValidateClaudeCredentials validates recognized OAuth documents without
// changing explicit alternative credential formats. Refresh-capable OAuth may
// have an expired or absent access token: the provider owns token refresh.
func ValidateClaudeCredentials(raw []byte) error {
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return ErrClaudeAuthUnavailable
	}
	oauthRaw, exists := document["claudeAiOauth"]
	if !exists {
		return nil
	}
	var oauth struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    *float64 `json:"expiresAt"`
	}
	if json.Unmarshal(oauthRaw, &oauth) != nil {
		return ErrClaudeAuthUnavailable
	}
	if strings.TrimSpace(oauth.RefreshToken) != "" {
		return nil
	}
	if strings.TrimSpace(oauth.AccessToken) == "" {
		return ErrClaudeAuthUnavailable
	}
	if oauth.ExpiresAt != nil && *oauth.ExpiresAt <= float64(time.Now().UnixMilli()) {
		return ErrClaudeAuthUnavailable
	}
	return nil
}

// ValidateClaudeCredentialsFile deliberately hides filesystem errors too.
func ValidateClaudeCredentialsFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ErrClaudeAuthUnavailable
	}
	return ValidateClaudeCredentials(raw)
}
