// Package agentsessions manages isolated agent home directories for session
// preservation. Each agentruntime session gets its own ~/.claude/ or ~/.codex/
// mounted into the container, so sessions are captured live and resumable.
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

// claudeSessionEntry mirrors the JSON structure Claude Code writes to
// ~/.claude/sessions/{pid}.json for session discovery.
type claudeSessionEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
}

// MangleProjectPath converts an absolute path to Claude's project directory name.
// Claude replaces "/" with "-" and keeps the leading "-".
// Example: "/Users/me/project" → "-Users-me-project"
func MangleProjectPath(absPath string) string {
	return strings.ReplaceAll(absPath, "/", "-")
}

// InitClaudeSessionDir creates an isolated ~/.claude/ directory structure for a
// single agentruntime session. Returns the path to mount as /root/.claude/ (rw).
//
// Structure created:
//
//	{dataDir}/claude-sessions/{sessionID}/
//	  projects/{mangled-project-path}/     ← Claude writes .jsonl here
//	  sessions/                            ← session discovery index
//	  credentials.json                     ← copied from credentialsPath if set
//	  .credentials.json                    ← legacy-compatible credentials copy
func InitClaudeSessionDir(dataDir, sessionID, projectPath, credentialsPath string) (string, error) {
	sessionDir := filepath.Join(dataDir, "claude-sessions", sessionID)

	// Create the project subdirectory where Claude will write session .jsonl files.
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		absProject = projectPath
	}
	projectDir := filepath.Join(sessionDir, "projects", MangleProjectPath(absProject))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return "", fmt.Errorf("create project dir: %w", err)
	}

	// Create sessions directory for PID-based discovery.
	sessionsDir := filepath.Join(sessionDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return "", fmt.Errorf("create sessions dir: %w", err)
	}

	// Copy credentials if provided.
	if credentialsPath != "" {
		expandedCreds := expandHome(credentialsPath)
		if data, err := os.ReadFile(expandedCreds); err == nil {
			for _, name := range []string{"credentials.json", ".credentials.json"} {
				credsTarget := filepath.Join(sessionDir, name)
				if err := os.WriteFile(credsTarget, data, 0o600); err != nil {
					return "", fmt.Errorf("write credentials: %w", err)
				}
			}
		}
		// Silently skip if credentials file doesn't exist — caller may provide
		// the path before the file is synced from Keychain.
	}

	// NOTE: Do NOT copy the host's ~/.claude.json here. It contains personal
	// account state (orgId, preferences, project trust for host paths) that
	// should not leak into agent containers. The materializer writes a
	// controlled .claude.json with only the fields needed to skip onboarding
	// and pre-trust /workspace.

	return sessionDir, nil
}

// ReadLastClaudeSessionID finds the most recent Claude session ID from a
// session directory. It checks two sources:
//  1. sessions/*.json (PID-based index) — sorted by startedAt
//  2. Fallback: scan projects/{hash}/*.jsonl by mtime
//
// Returns ("", nil) if no session found (first run).
func ReadLastClaudeSessionID(sessionDir string) (string, error) {
	// Try sessions/*.json first.
	sessionsDir := filepath.Join(sessionDir, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err == nil && len(entries) > 0 {
		var sessions []claudeSessionEntry
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(sessionsDir, e.Name()))
			if err != nil {
				continue
			}
			var entry claudeSessionEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				continue
			}
			sessions = append(sessions, entry)
		}
		if len(sessions) > 0 {
			sort.Slice(sessions, func(i, j int) bool {
				return sessions[i].StartedAt > sessions[j].StartedAt
			})
			return sessions[0].SessionID, nil
		}
	}

	// Fallback: scan for .jsonl files by mtime.
	projectsDir := filepath.Join(sessionDir, "projects")
	var newest string
	var newestTime time.Time
	filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = strings.TrimSuffix(info.Name(), ".jsonl")
		}
		return nil
	})

	return newest, nil
}

// ClaudeResumeArgs returns the CLI flags needed to resume a prior Claude session.
// Returns nil if no prior session found (first run — no resume needed).
func ClaudeResumeArgs(dataDir, agentRuntimeSessionID string) ([]string, error) {
	sessionDir := filepath.Join(dataDir, "claude-sessions", agentRuntimeSessionID)
	sessionID, err := ReadLastClaudeSessionID(sessionDir)
	if err != nil {
		return nil, err
	}
	if sessionID == "" {
		return nil, nil
	}
	return []string{"--resume", "--session-id", sessionID}, nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
