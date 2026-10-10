package materialize

import (
	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeMaterializationRejectsUnusableOAuth(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "cache", true: "explicit"}[explicit], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "credentials", "claude-credentials.json")
			os.MkdirAll(filepath.Dir(path), 0700)
			os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`), 0600)
			req := &apischema.SessionRequest{Agent: "claude", Claude: &apischema.ClaudeConfig{}}
			if explicit {
				req.Claude.CredentialsPath = path
			}
			result, err := Materialize(req, "invalid-auth", dir)
			if err == nil || result != nil {
				t.Fatal("unusable OAuth materialized for provider launch")
			}
			if err.Error() != "Claude authentication unavailable: invalid credentials" {
				t.Fatalf("unsanitized error: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "claude-sessions", "invalid-auth")); !os.IsNotExist(err) {
				t.Fatal("provider session directory created before auth validation")
			}
		})
	}
}
