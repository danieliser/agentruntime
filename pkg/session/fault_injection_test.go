package session

import (
	"context"
	"sync"
	"testing"

	"github.com/danieliser/agentruntime/pkg/runtime"
)

// --- Fault injection: nil and zero-value inputs ---

func TestSession_SetRunning_NilHandle(t *testing.T) {
	s := NewSession("", "claude", "local")
	// Must not panic with nil handle.
	s.SetRunning(nil)
	if s.State != StateRunning {
		t.Fatalf("expected StateRunning, got %q", s.State)
	}
}

func TestSession_SetCompleted_WithoutSetRunning(t *testing.T) {
	// Calling SetCompleted on a Pending session — shouldn't panic.
	s := NewSession("", "claude", "local")
	s.SetCompleted(0)
	if s.State != StateCompleted {
		t.Fatalf("expected StateCompleted, got %q", s.State)
	}
}

func TestSession_Kill_NoHandle(t *testing.T) {
	s := NewSession("", "claude", "local")
	// Kill on a session with no handle — must not panic.
	err := s.Kill()
	if err != nil {
		t.Fatalf("expected nil error for Kill with no handle, got %v", err)
	}
}

func TestSession_Snapshot_BeforeRunning(t *testing.T) {
	s := NewSession("task-1", "codex", "docker")
	snap := s.Snapshot()
	if snap.State != StatePending {
		t.Fatalf("expected pending in snapshot, got %q", snap.State)
	}
	if snap.ID != s.ID {
		t.Fatal("snapshot ID mismatch")
	}
}

func TestSession_Snapshot_AfterCompletion(t *testing.T) {
	s := NewSession("", "claude", "local")
	s.SetRunning(nil)
	s.SetCompleted(42)
	snap := s.Snapshot()
	if snap.State != StateFailed {
		t.Fatalf("expected StateFailed, got %q", snap.State)
	}
	if snap.ExitCode == nil || *snap.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %v", snap.ExitCode)
	}
}

func TestSession_DoubleSetCompleted(t *testing.T) {
	// Calling SetCompleted twice — second call should overwrite, not panic.
	s := NewSession("", "claude", "local")
	s.SetRunning(nil)
	s.SetCompleted(0)
	s.SetCompleted(1)
	if s.State != StateFailed {
		t.Fatalf("expected StateFailed after second SetCompleted(1), got %q", s.State)
	}
}

// --- Invariant: state machine validity ---

func TestSession_StateTransitions_AllPaths(t *testing.T) {
	// Every valid path through the state machine.
	cases := []struct {
		name     string
		actions  func(*Session)
		expected State
	}{
		{"pending only", func(s *Session) {}, StatePending},
		{"pending → running", func(s *Session) { s.SetRunning(nil) }, StateRunning},
		{"pending → running → completed", func(s *Session) {
			s.SetRunning(nil)
			s.SetCompleted(0)
		}, StateCompleted},
		{"pending → running → failed", func(s *Session) {
			s.SetRunning(nil)
			s.SetCompleted(1)
		}, StateFailed},
		{"skip running → completed", func(s *Session) {
			s.SetCompleted(0)
		}, StateCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession("", "claude", "local")
			tc.actions(s)
			if s.State != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, s.State)
			}
		})
	}
}

// --- Concurrent fault injection ---

func TestSession_ConcurrentSnapshotDuringCompletion(t *testing.T) {
	// Snapshot while SetCompleted is being called concurrently.
	// Must never see corrupted state.
	s := NewSession("", "claude", "local")
	s.SetRunning(nil)

	var wg sync.WaitGroup
	// 10 completers.
	for i := 0; i < 10; i++ {
		code := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetCompleted(code)
		}()
	}
	// 10 snapshotters.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := s.Snapshot()
			// State must be a valid enum value.
			switch snap.State {
			case StatePending, StateRunning, StateCompleted, StateFailed, StateOrphaned:
				// ok
			default:
				t.Errorf("invalid state in snapshot: %q", snap.State)
			}
		}()
	}
	wg.Wait()
}

// --- Manager fault injection ---

func TestManager_AddNilSession(t *testing.T) {
	// This is a programming error but must not corrupt the map.
	// Note: passing nil to Add would panic on s.ID access — that's acceptable
	// as it's a programmer bug, not a user input issue. Skip if desired.
	// This test documents the expected behavior.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when adding nil session")
		}
	}()
	m := NewManager()
	m.Add(nil)
}

func TestManager_RecoverWithMixedHandles(t *testing.T) {
	m := NewManager()
	// Create two handles — one real, one will be used for recovery.
	rt := runtime.NewLocalRuntime()
	ctx := context.Background()
	h1, err := rt.Spawn(ctx, runtime.SpawnConfig{Cmd: []string{"sleep", "1"}})
	if err != nil {
		t.Skip("can't spawn sleep")
	}
	defer h1.Kill()

	h2, err := rt.Spawn(ctx, runtime.SpawnConfig{Cmd: []string{"sleep", "1"}})
	if err != nil {
		t.Skip("can't spawn sleep")
	}
	defer h2.Kill()

	recovered := m.Recover([]runtime.ProcessHandle{h1, h2}, "test")
	if len(recovered) != 2 {
		t.Fatalf("expected 2 recovered, got %d", len(recovered))
	}

	// Both should be independently addressable.
	if recovered[0].ID == recovered[1].ID {
		t.Fatal("recovered sessions should have unique IDs")
	}
}

