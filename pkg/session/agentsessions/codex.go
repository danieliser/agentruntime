package agentsessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// codexSessionMeta mirrors the metadata Codex writes to session files.
type codexSessionMeta struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at,omitempty"`
}

// InitCodexSessionDir creates an isolated ~/.codex/ directory structure.
// Returns the path to mount as /root/.codex/ (rw).
func InitCodexSessionDir(dataDir, sessionID string) (string, error) {
	sessionDir := filepath.Join(dataDir, "codex-sessions", sessionID)
	sessionsDir := filepath.Join(sessionDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return "", fmt.Errorf("create codex sessions dir: %w", err)
	}
	return sessionDir, nil
}

// ReadLastCodexSessionID finds the most recent Codex session ID from a
// session directory. Codex writes to sessions/*.json or sessions/YYYY/MM/DD/*.jsonl.
func ReadLastCodexSessionID(sessionDir string) (string, error) {
	sessionsDir := filepath.Join(sessionDir, "sessions")

	var newest string
	var newestTime time.Time

	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(info.Name())
		if ext != ".json" && ext != ".jsonl" {
			return nil
		}

		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()

			// Try to read session ID from file content.
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				var meta codexSessionMeta
				if json.Unmarshal(data, &meta) == nil && meta.ID != "" {
					newest = meta.ID
					return nil
				}
			}
			// Fallback: use filename without extension.
			newest = strings.TrimSuffix(info.Name(), ext)
		}
		return nil
	})

	return newest, nil
}

// CodexResumeArgs returns the CLI flags needed to resume a prior Codex session.
// Returns nil if no prior session found.
func CodexResumeArgs(dataDir, agentRuntimeSessionID string) ([]string, error) {
	sessionDir := filepath.Join(dataDir, "codex-sessions", agentRuntimeSessionID)
	sessionID, err := ReadLastCodexSessionID(sessionDir)
	if err != nil {
		return nil, err
	}
	if sessionID == "" {
		return nil, nil
	}
	// Codex resume flag — verify against actual CLI.
	return []string{"--session", sessionID}, nil
}

// PruneOldSessions deletes session directories older than the retention window.
// Sweeps both claude-sessions/ and codex-sessions/.
func PruneOldSessions(dataDir string, retention time.Duration) error {
	cutoff := time.Now().Add(-retention)

	for _, subdir := range []string{"claude-sessions", "codex-sessions"} {
		baseDir := filepath.Join(dataDir, subdir)
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read %s: %w", subdir, err)
		}

		// Sort by name for deterministic pruning.
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})

		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				path := filepath.Join(baseDir, e.Name())
				if err := os.RemoveAll(path); err != nil {
					return fmt.Errorf("prune %s: %w", path, err)
				}
			}
		}
	}
	return nil
}
