package docker_test

import (
	"os"
	"strings"
	"testing"
)

// The Docker CLI pin must match the host version used to qualify current
// ChatGPT model support. A source change must update this qualification guard.
func TestCodexImageUsesQualifiedVersion(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile.agent")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for _, provider := range []string{"all)", "codex)"} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, provider) {
				found = true
				if !strings.Contains(line, "@openai/codex@0.162.0") || strings.Contains(line, "@openai/codex@latest") {
					t.Fatalf("%s must install qualified Codex 0.162.0", provider)
				}
			}
		}
		if !found {
			t.Fatalf("missing %s provider recipe", provider)
		}
	}
}
