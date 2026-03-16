// Package credentials provides credential sync utilities for agent runtimes.
// It extracts OAuth tokens and API keys from platform-specific credential
// stores (macOS Keychain, Linux manual placement) and caches them as files
// that can be bind-mounted into containers.
//
// agentruntime owns credential sync so every consumer doesn't have to solve
// it independently. The caller can also bypass this entirely by providing
// credentials_path directly in ClaudeConfig.
package credentials

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sync manages credential extraction and caching.
type Sync struct {
	dataDir   string
	extractor tokenExtractor
	mu        sync.Mutex
}

// tokenExtractor abstracts platform-specific credential extraction.
// Implementations: keychainExtractor (macOS), fileExtractor (Linux fallback).
type tokenExtractor interface {
	Extract(service string) (string, error)
}

// NewSync creates a credential sync manager. DataDir is typically
// ~/.local/share/agentruntime — credentials are cached under {dataDir}/credentials/.
func NewSync(dataDir string) *Sync {
	return &Sync{
		dataDir:   dataDir,
		extractor: platformExtractor(),
	}
}

// newSyncWithExtractor is for testing — injects a mock extractor.
func newSyncWithExtractor(dataDir string, ext tokenExtractor) *Sync {
	return &Sync{
		dataDir:   dataDir,
		extractor: ext,
	}
}

const (
	credentialsSubdir = "credentials"
	throttleInterval  = 30 * time.Second
	claudeService     = "Claude Code-credentials"
	claudeCacheFile   = "claude-credentials.json"
)

// ClaudeCredentialsFile returns the path to a cached Claude OAuth credentials
// file. If the cache is fresh (< 30s old), returns immediately. Otherwise
// extracts from the platform credential store and writes to cache.
//
// The returned path can be passed directly to ClaudeConfig.CredentialsPath.
func (s *Sync) ClaudeCredentialsFile() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cacheDir := filepath.Join(s.dataDir, credentialsSubdir)
	cachePath := filepath.Join(cacheDir, claudeCacheFile)

	// Check cache freshness.
	if info, err := os.Stat(cachePath); err == nil {
		if time.Since(info.ModTime()) < throttleInterval {
			return cachePath, nil
		}
	}

	// Extract from platform credential store.
	raw, err := s.extractor.Extract(claudeService)
	if err != nil {
		// If extraction fails but cache exists, return stale cache.
		if _, statErr := os.Stat(cachePath); statErr == nil {
			return cachePath, nil
		}
		return "", err
	}

	// Write to cache.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(cachePath, []byte(raw), 0o600); err != nil {
		return "", err
	}

	return cachePath, nil
}

// CodexCredentialsFile returns the path to the Codex auth.json file.
// Codex uses OAuth by default (~/.codex/auth.json). If that file exists,
// return its path. Otherwise fall back to OPENAI_API_KEY env var (API key mode).
//
// For containers: mount the returned path as /root/.codex/auth.json (ro).
func (s *Sync) CodexCredentialsFile() (string, error) {
	// Check default Codex auth location.
	home, err := os.UserHomeDir()
	if err == nil {
		authPath := filepath.Join(home, ".codex", "auth.json")
		if _, err := os.Stat(authPath); err == nil {
			return authPath, nil
		}
	}
	return "", fmt.Errorf("codex auth.json not found at ~/.codex/auth.json")
}

// CodexAPIKey returns the OpenAI API key for Codex (API key mode).
// Checks OPENAI_API_KEY env first, falls back to ANTHROPIC_API_KEY
// (some Codex configs use Anthropic models via OpenAI-compatible API).
func (s *Sync) CodexAPIKey() (string, error) {
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return key, nil
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("neither OPENAI_API_KEY nor ANTHROPIC_API_KEY set")
}

// Watch starts a background goroutine that refreshes Claude credentials
// on the given interval. Cancel the context to stop.
func (s *Sync) Watch(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.ClaudeCredentialsFile() // ignore error — best effort
			}
		}
	}()
}
