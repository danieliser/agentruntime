package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CursorStore keeps independent sequence cursors for each caller/session pair.
type CursorStore struct{ Dir string }

func (s CursorStore) path(caller, session string) string {
	h := sha256.Sum256([]byte(caller + "\x00" + session))
	return filepath.Join(s.Dir, hex.EncodeToString(h[:])+".json")
}

func (s CursorStore) Load(caller, session string) (int64, error) {
	b, err := os.ReadFile(s.path(caller, session))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read cursor: %w", err)
	}
	var value struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal(b, &value); err != nil {
		return 0, fmt.Errorf("decode cursor: %w", err)
	}
	if value.Sequence < 0 {
		return 0, fmt.Errorf("cursor sequence is negative")
	}
	return value.Sequence, nil
}

func (s CursorStore) Save(caller, session string, sequence int64) error {
	if sequence < 0 {
		return fmt.Errorf("cursor sequence is negative")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create cursor directory: %w", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return fmt.Errorf("secure cursor directory: %w", err)
	}
	path := s.path(caller, session)
	f, err := os.CreateTemp(s.Dir, ".cursor-*.tmp")
	if err != nil {
		return fmt.Errorf("create cursor temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		_, err = fmt.Fprintf(f, "{\"sequence\":%d}\n", sequence)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish cursor: %w", err)
	}
	return nil
}

func cursorStoreFrom(env map[string]string, override string) (CursorStore, error) {
	dir := override
	if dir == "" {
		dir = env["AGENTCTL_CURSOR_DIR"]
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return CursorStore{}, err
		}
		dir = filepath.Join(home, ".agentd", "agentctl", "cursors")
	}
	return CursorStore{Dir: dir}, nil
}
