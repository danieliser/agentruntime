// Package config resolves AgentD's process-wide configuration defaults.
package config

import (
	"os"
	"path/filepath"
)

// DataDir returns the unified private state root used by the daemon and its
// first-party local clients.
func DataDir() string {
	if directory := os.Getenv("AGENTRUNTIME_DATA_DIR"); directory != "" {
		return directory
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentd")
}
