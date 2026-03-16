//go:build linux

package credentials

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// linuxExtractor tries secret-tool first, then falls back to checking
// the cached credentials file directly (manual placement).
type linuxExtractor struct {
	dataDir string
}

func (e *linuxExtractor) Extract(service string) (string, error) {
	// Try secret-tool (GNOME Keyring / KDE Wallet).
	if path, err := exec.LookPath("secret-tool"); err == nil {
		cmd := exec.Command(path, "lookup", "service", service)
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}

	// Fallback: check for manually placed credentials file.
	// Document this as the Linux path: users copy from Mac or generate via OAuth flow.
	cacheFile := fmt.Sprintf("%s/credentials/claude-credentials.json", e.dataDir)
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return "", fmt.Errorf("no credential store available and no cached file at %s: %w", cacheFile, err)
	}
	return string(data), nil
}

func platformExtractor() tokenExtractor {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = home + "/.local/share/agentruntime"
	}
	return &linuxExtractor{dataDir: dataDir}
}
