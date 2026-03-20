package credentials

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CheckCodexTokenExpiry reads the Codex auth.json and determines if the access token
// needs refresh. It returns the token expiry time, whether refresh is needed, and any error.
//
// Returns needsRefresh=true if the token expires within 24 hours.
func CheckCodexTokenExpiry(authPath string) (expiresAt time.Time, needsRefresh bool, err error) {
	// Read auth.json
	data, err := os.ReadFile(authPath)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read auth.json: %w", err)
	}

	var auth struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return time.Time{}, false, fmt.Errorf("parse auth.json: %w", err)
	}

	if auth.AccessToken == "" {
		return time.Time{}, false, fmt.Errorf("access_token not found in auth.json")
	}

	// Decode JWT payload (format: header.payload.signature)
	parts := strings.Split(auth.AccessToken, ".")
	if len(parts) != 3 {
		return time.Time{}, false, fmt.Errorf("invalid JWT format")
	}

	// Decode payload (base64url)
	payload := parts[1]
	// Add padding if needed
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("decode JWT payload: %w", err)
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return time.Time{}, false, fmt.Errorf("parse JWT claims: %w", err)
	}

	if claims.Exp == 0 {
		return time.Time{}, false, fmt.Errorf("exp claim not found in token")
	}

	expiresAt = time.Unix(claims.Exp, 0)
	now := time.Now()

	// needsRefresh if expires within 24 hours
	needsRefresh = expiresAt.Sub(now) < 24*time.Hour

	return expiresAt, needsRefresh, nil
}

// RefreshCodexToken reads the Codex auth.json, extracts the refresh_token,
// and calls the OpenAI token endpoint to obtain new access/refresh tokens.
// On success, writes the updated tokens back to auth.json (atomic write, 0o600).
// On failure, logs a warning with remediation instructions but does not crash.
func RefreshCodexToken(authPath string) error {
	// Read current auth.json
	data, err := os.ReadFile(authPath)
	if err != nil {
		return fmt.Errorf("read auth.json: %w", err)
	}

	var auth struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return fmt.Errorf("parse auth.json: %w", err)
	}

	if auth.RefreshToken == "" {
		return fmt.Errorf("refresh_token not found in auth.json")
	}

	// Extract client ID from id_token JWT payload (look for "azp" or "aud" claim)
	clientID := extractClientID(auth.IDToken)
	if clientID == "" {
		// Fallback to known client ID
		clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	}

	// Determine token endpoint from issuer claim
	tokenEndpoint := "https://auth0.openai.com/oauth/token"
	if iss := extractIssuer(auth.IDToken); iss != "" {
		// Construct endpoint from issuer (e.g., https://auth0.openai.com/ -> https://auth0.openai.com/oauth/token)
		tokenEndpoint = strings.TrimSuffix(iss, "/") + "/oauth/token"
	}

	// POST to token endpoint
	formData := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {auth.RefreshToken},
		"client_id":     {clientID},
	}

	resp, err := http.PostForm(tokenEndpoint, formData)
	if err != nil {
		logRefreshWarning("network error", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logRefreshWarning(fmt.Sprintf("HTTP %d", resp.StatusCode), fmt.Errorf("%s", string(body)))
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var newAuth struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&newAuth); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}

	// Merge with existing fields (preserve fields not returned by refresh)
	if newAuth.AccessToken == "" {
		return fmt.Errorf("access_token not in token response")
	}

	auth.AccessToken = newAuth.AccessToken
	if newAuth.RefreshToken != "" {
		auth.RefreshToken = newAuth.RefreshToken
	}
	if newAuth.IDToken != "" {
		auth.IDToken = newAuth.IDToken
	}
	if newAuth.ExpiresIn > 0 {
		auth.ExpiresIn = newAuth.ExpiresIn
	}
	if newAuth.Scope != "" {
		auth.Scope = newAuth.Scope
	}
	if newAuth.TokenType != "" {
		auth.TokenType = newAuth.TokenType
	}

	// Atomic write: write to temp file, then rename
	updatedData, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal updated auth: %w", err)
	}

	dir := filepath.Dir(authPath)
	tmpFile, err := os.CreateTemp(dir, ".codex-auth-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name()) // cleanup on error

	if _, err := tmpFile.Write(updatedData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()

	// Set restrictive permissions before rename
	if err := os.Chmod(tmpFile.Name(), 0o600); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpFile.Name(), authPath); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}

// extractClientID extracts the "azp" or "aud" claim from an id_token JWT.
func extractClientID(idToken string) string {
	if idToken == "" {
		return ""
	}

	payload := extractJWTPayload(idToken)
	if payload == nil {
		return ""
	}

	// Try "azp" first, fall back to "aud"
	if azp, ok := payload["azp"].(string); ok && azp != "" {
		return azp
	}
	if aud, ok := payload["aud"].(string); ok && aud != "" {
		return aud
	}

	return ""
}

// extractIssuer extracts the "iss" claim from an id_token JWT.
func extractIssuer(idToken string) string {
	if idToken == "" {
		return ""
	}

	payload := extractJWTPayload(idToken)
	if payload == nil {
		return ""
	}

	if iss, ok := payload["iss"].(string); ok {
		return iss
	}

	return ""
}

// extractJWTPayload decodes a JWT payload and returns it as a map.
func extractJWTPayload(token string) map[string]interface{} {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}

	payload := parts[1]
	// Add padding if needed
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil
	}

	return claims
}

// logRefreshWarning logs a warning with remediation instructions.
func logRefreshWarning(reason string, err error) {
	log.Printf("warning: codex token refresh failed (%s: %v) — run 'codex login' to refresh credentials", reason, err)
}
