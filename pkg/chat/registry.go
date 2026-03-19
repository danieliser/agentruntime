package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// ErrNotFound is returned when a chat record does not exist.
var ErrNotFound = errors.New("chat not found")

// namePattern enforces: starts with [a-z0-9], followed by [a-z0-9-_]*, max 64 chars.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

const maxNameLen = 64

// ValidateName checks that name is a valid chat name.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("chat name must not be empty")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("chat name must be at most %d characters", maxNameLen)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("chat name must match %s", namePattern.String())
	}
	return nil
}

// Registry provides file-based CRUD for ChatRecord.
// Each chat is stored as {dir}/{name}.json.
type Registry struct {
	dir string
}

// NewRegistry creates a Registry backed by {dataDir}/chats/.
// It creates the directory if it does not exist.
func NewRegistry(dataDir string) (*Registry, error) {
	dir := filepath.Join(dataDir, "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create chats dir: %w", err)
	}
	return &Registry{dir: dir}, nil
}

// Save writes the record atomically (write to .tmp, rename to .json).
func (r *Registry) Save(rec *ChatRecord) error {
	if err := ValidateName(rec.Name); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chat record: %w", err)
	}
	tmp := r.path(rec.Name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, r.path(rec.Name)); err != nil {
		os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// Load reads a chat record by name. Returns ErrNotFound if absent.
func (r *Registry) Load(name string) (*ChatRecord, error) {
	data, err := os.ReadFile(r.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read chat file: %w", err)
	}
	var rec ChatRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal chat record: %w", err)
	}
	return &rec, nil
}

// List returns all chat records sorted by CreatedAt ascending.
func (r *Registry) List() ([]*ChatRecord, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, fmt.Errorf("read chats dir: %w", err)
	}
	var records []*ChatRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".json")]
		rec, err := r.Load(name)
		if err != nil {
			continue // skip corrupt files
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	return records, nil
}

// Delete removes the chat record file. Returns ErrNotFound if absent.
func (r *Registry) Delete(name string) error {
	err := os.Remove(r.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("remove chat file: %w", err)
	}
	return nil
}

// Exists reports whether a chat with the given name exists.
func (r *Registry) Exists(name string) bool {
	_, err := os.Stat(r.path(name))
	return err == nil
}

// path returns the filesystem path for a chat record.
func (r *Registry) path(name string) string {
	return filepath.Join(r.dir, name+".json")
}
