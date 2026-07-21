package agent

import "fmt"

// RedactPrompt returns a copy of cmd with any argv element equal to prompt
// replaced by a placeholder. Daemon logs must never persist prompt text —
// prompts can carry pasted tokens, signed URLs, or proprietary data.
func RedactPrompt(cmd []string, prompt string) []string {
	if prompt == "" {
		return cmd
	}
	out := append([]string(nil), cmd...)
	for i, arg := range out {
		if arg == prompt {
			out[i] = fmt.Sprintf("[prompt: %d bytes]", len(prompt))
		}
	}
	return out
}
