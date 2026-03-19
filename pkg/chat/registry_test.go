package chat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func makeRecord(name string, createdAt time.Time) *ChatRecord {
	return &ChatRecord{
		Name:         name,
		Config:       ChatConfig{Agent: "claude"},
		State:        ChatStateCreated,
		SessionChain: []string{},
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
}

func TestRegistry_SaveLoad_RoundTrip(t *testing.T) {
	reg := newTestRegistry(t)
	now := time.Now().Truncate(time.Millisecond) // JSON loses sub-ms precision
	lastActive := now.Add(-5 * time.Minute)

	original := &ChatRecord{
		Name:           "web-ui",
		Config:         ChatConfig{Agent: "claude", Runtime: "docker", IdleTimeout: "1h", Env: map[string]string{"FOO": "bar"}},
		State:          ChatStateRunning,
		VolumeName:     "agentruntime-chat-web-ui",
		CurrentSession: "sess-1",
		SessionChain:   []string{"sess-0", "sess-1"},
		PendingMessage: "hello",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActiveAt:   &lastActive,
	}

	if err := reg.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := reg.Load("web-ui")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Verify all fields round-trip.
	if loaded.Name != original.Name {
		t.Errorf("Name = %q, want %q", loaded.Name, original.Name)
	}
	if loaded.Config.Agent != original.Config.Agent {
		t.Errorf("Config.Agent = %q, want %q", loaded.Config.Agent, original.Config.Agent)
	}
	if loaded.Config.Runtime != original.Config.Runtime {
		t.Errorf("Config.Runtime = %q, want %q", loaded.Config.Runtime, original.Config.Runtime)
	}
	if loaded.Config.IdleTimeout != original.Config.IdleTimeout {
		t.Errorf("Config.IdleTimeout = %q, want %q", loaded.Config.IdleTimeout, original.Config.IdleTimeout)
	}
	if loaded.Config.Env["FOO"] != "bar" {
		t.Errorf("Config.Env[FOO] = %q, want bar", loaded.Config.Env["FOO"])
	}
	if loaded.State != original.State {
		t.Errorf("State = %q, want %q", loaded.State, original.State)
	}
	if loaded.VolumeName != original.VolumeName {
		t.Errorf("VolumeName = %q, want %q", loaded.VolumeName, original.VolumeName)
	}
	if loaded.CurrentSession != original.CurrentSession {
		t.Errorf("CurrentSession = %q, want %q", loaded.CurrentSession, original.CurrentSession)
	}
	if len(loaded.SessionChain) != 2 || loaded.SessionChain[1] != "sess-1" {
		t.Errorf("SessionChain = %v, want [sess-0 sess-1]", loaded.SessionChain)
	}
	if loaded.PendingMessage != original.PendingMessage {
		t.Errorf("PendingMessage = %q, want %q", loaded.PendingMessage, original.PendingMessage)
	}
	if !loaded.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", loaded.CreatedAt, original.CreatedAt)
	}
	if loaded.LastActiveAt == nil || !loaded.LastActiveAt.Equal(*original.LastActiveAt) {
		t.Errorf("LastActiveAt = %v, want %v", loaded.LastActiveAt, original.LastActiveAt)
	}
}

func TestRegistry_Load_NotFound(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := reg.Load("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Load nonexistent = %v, want ErrNotFound", err)
	}
}

func TestRegistry_List_Sorted(t *testing.T) {
	reg := newTestRegistry(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Save in non-chronological order.
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"chat-c", 2 * time.Hour},
		{"chat-a", 0},
		{"chat-b", 1 * time.Hour},
	} {
		if err := reg.Save(makeRecord(tc.name, base.Add(tc.offset))); err != nil {
			t.Fatalf("Save %s: %v", tc.name, err)
		}
	}

	list, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	want := []string{"chat-a", "chat-b", "chat-c"}
	for i, rec := range list {
		if rec.Name != want[i] {
			t.Errorf("List[%d].Name = %q, want %q", i, rec.Name, want[i])
		}
	}
}

func TestRegistry_Delete(t *testing.T) {
	reg := newTestRegistry(t)
	rec := makeRecord("doomed", time.Now())
	if err := reg.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := reg.Delete("doomed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := reg.Load("doomed")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Load after Delete = %v, want ErrNotFound", err)
	}
}

func TestRegistry_Delete_NotFound(t *testing.T) {
	reg := newTestRegistry(t)
	err := reg.Delete("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete nonexistent = %v, want ErrNotFound", err)
	}
}

func TestRegistry_Exists(t *testing.T) {
	reg := newTestRegistry(t)
	if reg.Exists("nope") {
		t.Error("Exists(nope) = true before save")
	}
	if err := reg.Save(makeRecord("present", time.Now())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !reg.Exists("present") {
		t.Error("Exists(present) = false after save")
	}
}

func TestValidateName_Valid(t *testing.T) {
	valid := []string{"web-ui", "chat1", "my_chat", "a", "0abc"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateName_Invalid(t *testing.T) {
	invalid := []string{
		"",                                   // empty
		"Web-UI",                             // uppercase
		"a b",                                // space
		"-bad",                               // leading hyphen
		"_bad",                               // leading underscore
		strings.Repeat("a", maxNameLen+1),    // too long
		"hello world",                        // spaces
		"café",                               // non-ASCII
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}
}

func TestRegistry_AtomicWrite(t *testing.T) {
	reg := newTestRegistry(t)

	// Save a valid record first.
	original := makeRecord("atomic", time.Now().Truncate(time.Millisecond))
	if err := reg.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate a failed mid-write by leaving a .tmp file and verifying
	// the original .json is intact.
	tmpPath := filepath.Join(reg.dir, "atomic.json.tmp")
	if err := os.WriteFile(tmpPath, []byte("corrupt garbage"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	// Original should still load cleanly.
	loaded, err := reg.Load("atomic")
	if err != nil {
		t.Fatalf("Load after leftover tmp: %v", err)
	}
	if loaded.Name != "atomic" {
		t.Errorf("Name = %q, want atomic", loaded.Name)
	}

	// A new Save should overwrite the stale .tmp and succeed.
	original.State = ChatStateRunning
	if err := reg.Save(original); err != nil {
		t.Fatalf("Save over stale tmp: %v", err)
	}
	loaded, err = reg.Load("atomic")
	if err != nil {
		t.Fatalf("Load after re-save: %v", err)
	}
	if loaded.State != ChatStateRunning {
		t.Errorf("State = %q, want running", loaded.State)
	}
}
