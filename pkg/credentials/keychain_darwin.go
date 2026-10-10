//go:build darwin

package credentials

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

// keychainExtractor reads credentials from the macOS Keychain.
type keychainExtractor struct{}

func (e *keychainExtractor) Extract(service string) (string, error) {
	account := claudeKeychainAccount(os.Getenv("USER"), func() (string, error) {
		current, err := user.Current()
		if err != nil {
			return "", err
		}
		return current.Username, nil
	})
	// A locked keychain or unanswered macOS prompt must not stall sync forever.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-a", account, "-s", service, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("keychain extraction failed for %q: %w", service, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func platformExtractor() tokenExtractor {
	return &keychainExtractor{}
}
