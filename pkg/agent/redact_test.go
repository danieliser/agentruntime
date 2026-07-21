package agent

import "testing"

func TestRedactPrompt(t *testing.T) {
	prompt := "read this signed URL https://example.com?sig=abc"
	cmd := []string{"claude", "-p", prompt, "--verbose"}

	got := RedactPrompt(cmd, prompt)

	for _, arg := range got {
		if arg == prompt {
			t.Fatalf("prompt not redacted: %v", got)
		}
	}
	if got[0] != "claude" || got[3] != "--verbose" {
		t.Fatalf("non-prompt args must be preserved: %v", got)
	}
	if cmd[2] != prompt {
		t.Fatalf("RedactPrompt must not mutate its input: %v", cmd)
	}
	if redacted := RedactPrompt(cmd, ""); &redacted[0] != &cmd[0] {
		t.Fatal("empty prompt should return cmd unchanged")
	}
}
