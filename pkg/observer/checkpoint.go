package observer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var ErrCheckpointNotFound = errors.New("observer checkpoint not found")

type Checkpoint struct {
	SessionID string `json:"session_id"`
	Sequence  int64  `json:"sequence"`
	EventID   string `json:"event_id"`
	TraceID   string `json:"trace_id,omitempty"`
}

type CheckpointStore struct {
	dir string
	mu  sync.Mutex
}

func NewCheckpointStore(dataDir, pluginName string) (*CheckpointStore, error) {
	if dataDir == "" || !safeName.MatchString(pluginName) {
		return nil, fmt.Errorf("observer: valid data directory and plugin name are required")
	}
	dir := filepath.Join(dataDir, "plugins", pluginName, "checkpoints")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("observer: create checkpoint directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("observer: secure checkpoint directory: %w", err)
	}
	return &CheckpointStore{dir: dir}, nil
}

func (store *CheckpointStore) path(sessionID string) string {
	return filepath.Join(store.dir, sessionID+".json")
}

func (store *CheckpointStore) Load(sessionID string) (Checkpoint, error) {
	if !safeName.MatchString(sessionID) {
		return Checkpoint{}, fmt.Errorf("observer: invalid checkpoint session ID")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.load(sessionID)
}

func (store *CheckpointStore) load(sessionID string) (Checkpoint, error) {
	data, err := os.ReadFile(store.path(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return Checkpoint{}, ErrCheckpointNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("observer: read checkpoint: %w", err)
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("observer: decode checkpoint: %w", err)
	}
	if checkpoint.SessionID != sessionID || checkpoint.Sequence < 1 || checkpoint.EventID == "" {
		return Checkpoint{}, fmt.Errorf("observer: corrupt checkpoint for session %q", sessionID)
	}
	return checkpoint, nil
}

func (store *CheckpointStore) Advance(checkpoint Checkpoint) error {
	if !safeName.MatchString(checkpoint.SessionID) || checkpoint.Sequence < 1 || checkpoint.EventID == "" {
		return fmt.Errorf("observer: invalid checkpoint")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := store.load(checkpoint.SessionID)
	if err == nil && (checkpoint.Sequence < current.Sequence || (checkpoint.Sequence == current.Sequence && checkpoint.EventID != current.EventID)) {
		return fmt.Errorf("observer: checkpoint regression or identity change from %d/%s to %d/%s", current.Sequence, current.EventID, checkpoint.Sequence, checkpoint.EventID)
	}
	if err == nil && current.TraceID != "" && checkpoint.TraceID != current.TraceID {
		return fmt.Errorf("observer: checkpoint trace linkage cannot change")
	}
	if err != nil && !errors.Is(err, ErrCheckpointNotFound) {
		return err
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("observer: encode checkpoint: %w", err)
	}
	tmp, err := os.CreateTemp(store.dir, ".checkpoint-*")
	if err != nil {
		return fmt.Errorf("observer: create checkpoint temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("observer: secure checkpoint temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("observer: write checkpoint: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("observer: sync checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("observer: close checkpoint: %w", err)
	}
	if err := os.Rename(tmpPath, store.path(checkpoint.SessionID)); err != nil {
		return fmt.Errorf("observer: publish checkpoint: %w", err)
	}
	directory, err := os.Open(store.dir)
	if err != nil {
		return fmt.Errorf("observer: open checkpoint directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("observer: sync checkpoint directory: %w", err)
	}
	return nil
}
