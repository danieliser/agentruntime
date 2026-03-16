//go:build darwin

package credentials

import (
	"fmt"
	"os/exec"
	"strings"
)

// keychainExtractor reads credentials from the macOS Keychain.
type keychainExtractor struct{}

func (e *keychainExtractor) Extract(service string) (string, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("keychain extraction failed for %q: %w", service, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func platformExtractor() tokenExtractor {
	return &keychainExtractor{}
}
