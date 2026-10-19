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
	sequence, _, err := s.Lookup(caller, session)
	return sequence, err
}

// Lookup is Load that also reports whether the caller has a saved cursor.
func (s CursorStore) Lookup(caller, session string) (int64, bool, error) {
	b, err := os.ReadFile(s.path(caller, session))
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read cursor: %w", err)
	}
	var value struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal(b, &value); err != nil {
		return 0, false, fmt.Errorf("decode cursor: %w", err)
	}
	if value.Sequence < 0 {
		return 0, false, fmt.Errorf("cursor sequence is negative")
	}
	return value.Sequence, true, nil
}

// waitKey and controlKey keep wait progress and the caller's last control mark
// apart from the since cursor, so waiting never hides output from since.
func waitKey(session string) string    { return session + "\x00wait" }
func controlKey(session string) string { return session + "\x00control" }

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

// Delete removes a saved cursor; a missing cursor is not an error.
func (s CursorStore) Delete(caller, session string) error {
	if err := os.Remove(s.path(caller, session)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cursor: %w", err)
	}
	return nil
}

// controlStoreFrom is the cursor store used to mark controls. It needs an
// explicit cursor directory or HOME in the given environment, so tests that
// pass a bare environment never write to the real home directory.
func controlStoreFrom(env map[string]string) *CursorStore {
	dir := env["AGENTCTL_CURSOR_DIR"]
	if dir == "" && env["HOME"] != "" {
		dir = filepath.Join(env["HOME"], ".agentd", "agentctl", "cursors")
	}
	if dir == "" {
		return nil
	}
	return &CursorStore{Dir: dir}
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

// commandCursorStore gives reads and explicit marks the same directory as
// automatic control marks, unless the command supplies --cursor-dir.
func commandCursorStore(ctx *Context, override string) (CursorStore, error) {
	if override == "" && ctx.Cursors != nil {
		return *ctx.Cursors, nil
	}
	return cursorStoreFrom(processEnv(), override)
}
