package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/runtime"
)

// --- Session lifecycle ---

func TestNewSession_InitialState(t *testing.T) {
	s := NewSession("task-1", "claude", "local")
	if s.State != StatePending {
		t.Fatalf("expected StatePending, got %q", s.State)
	}
	if s.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if s.AgentName != "claude" {
		t.Fatalf("expected AgentName 'claude', got %q", s.AgentName)
	}
	if s.RuntimeName != "local" {
		t.Fatalf("expected RuntimeName 'local', got %q", s.RuntimeName)
	}
	if s.Replay == nil {
		t.Fatal("expected non-nil replay buffer on new session")
	}
	if s.CreatedAt.IsZero() {
		t.Fatal("expected non-zero CreatedAt")
	}
}

func TestSession_SetRunning(t *testing.T) {
	s := NewSession("t1", "codex", "local")
	s.SetRunning(nil) // handle can be nil for state transition test
	if s.State != StateRunning {
		t.Fatalf("expected StateRunning, got %q", s.State)
	}
}

func TestSession_SetCompleted_ZeroExit(t *testing.T) {
	s := NewSession("t1", "claude", "local")
	s.SetRunning(nil)
	s.SetCompleted(0)
	if s.State != StateCompleted {
		t.Fatalf("expected StateCompleted for exit 0, got %q", s.State)
	}
	if s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("expected ExitCode=0, got %v", s.ExitCode)
	}
	if s.EndedAt == nil {
		t.Fatal("expected non-nil EndedAt after completion")
	}
}

func TestSession_SetCompleted_NonZeroExit(t *testing.T) {
	s := NewSession("t1", "claude", "local")
	s.SetRunning(nil)
	s.SetCompleted(1)
	if s.State != StateFailed {
		t.Fatalf("expected StateFailed for non-zero exit, got %q", s.State)
	}
	if s.ExitCode == nil || *s.ExitCode != 1 {
		t.Fatalf("expected ExitCode=1, got %v", s.ExitCode)
	}
}

func TestSession_SetCompleted_KillCode(t *testing.T) {
	s := NewSession("t1", "claude", "local")
	s.SetRunning(nil)
	s.SetCompleted(-1) // synthetic code from DELETE handler
	if s.State != StateFailed {
		t.Fatalf("expected StateFailed for kill exit, got %q", s.State)
	}
}

func TestSession_IDUnique(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s := NewSession("", "claude", "local")
		if ids[s.ID] {
			t.Fatalf("duplicate session ID: %s", s.ID)
		}
		ids[s.ID] = true
	}
}

// --- Manager CRUD ---

func TestManager_AddAndGet(t *testing.T) {
	m := NewManager()
	s := NewSession("", "claude", "local")
	if err := m.Add(s); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	got := m.Get(s.ID)
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.ID != s.ID {
		t.Fatalf("expected ID %q, got %q", s.ID, got.ID)
	}
}

func TestManager_GetMissing(t *testing.T) {
	m := NewManager()
	if m.Get("does-not-exist") != nil {
		t.Fatal("expected nil for unknown session")
	}
}

func TestManager_AddDuplicate(t *testing.T) {
	m := NewManager()
	s := NewSession("", "claude", "local")
	_ = m.Add(s)
	err := m.Add(s)
	if err == nil {
		t.Fatal("expected error on duplicate Add")
	}
}

func TestManager_Remove(t *testing.T) {
	m := NewManager()
	s := NewSession("", "claude", "local")
	_ = m.Add(s)
	m.Remove(s.ID)
	if m.Get(s.ID) != nil {
		t.Fatal("expected nil after Remove")
	}
}

func TestManager_RemoveMissing(t *testing.T) {
	// Must not panic on removing a non-existent ID.
	m := NewManager()
	m.Remove("no-such-id")
}

func TestManager_List(t *testing.T) {
	m := NewManager()
	for i := 0; i < 3; i++ {
		s := NewSession("", "claude", "local")
		_ = m.Add(s)
	}
	all := m.List()
	if len(all) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(all))
	}
}

func TestManager_ListEmpty(t *testing.T) {
	m := NewManager()
	all := m.List()
	if all == nil {
		t.Fatal("expected non-nil slice from empty manager")
	}
	if len(all) != 0 {
		t.Fatalf("expected empty list, got %d", len(all))
	}
}

// --- Concurrency ---

// TestManager_ConcurrentAddGet verifies that simultaneous reads and writes to the
// manager don't cause data races. Run with -race to catch violations.
func TestManager_ConcurrentAddGet(t *testing.T) {
	m := NewManager()
	var wg sync.WaitGroup

	// 10 writers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewSession("", "claude", "local")
			_ = m.Add(s)
		}()
	}

	// 10 readers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.List()
		}()
	}

	wg.Wait()
}

// TestManager_ConcurrentRemove verifies safe concurrent removal.
func TestManager_ConcurrentRemove(t *testing.T) {
	m := NewManager()
	sessions := make([]*Session, 20)
	for i := range sessions {
		s := NewSession("", "claude", "local")
		_ = m.Add(s)
		sessions[i] = s
	}

	var wg sync.WaitGroup
	for _, s := range sessions {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Remove(s.ID)
		}()
	}
	wg.Wait()

	if len(m.List()) != 0 {
		t.Fatalf("expected empty manager after removing all, got %d", len(m.List()))
	}
}

// TestSession_ConcurrentSetCompleted verifies that multiple goroutines calling
// SetCompleted simultaneously doesn't corrupt the session state. Only one write
// wins; the state must be a valid terminal state afterward.
func TestSession_ConcurrentSetCompleted(t *testing.T) {
	s := NewSession("", "claude", "local")
	s.SetRunning(nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		code := i % 2 // alternates 0 and 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetCompleted(code)
		}()
	}
	wg.Wait()

	// State must be one of the valid terminal states — not corrupted.
	if s.State != StateCompleted && s.State != StateFailed {
		t.Fatalf("expected terminal state, got %q", s.State)
	}
}

// --- Orphan recovery ---

func TestManager_Recover_EmptyHandles(t *testing.T) {
	m := NewManager()
	recovered := m.Recover(nil, "local")
	if len(recovered) != 0 {
		t.Fatalf("expected empty recovered list for nil handles, got %d", len(recovered))
	}
}

func TestManager_Recover_SetsOrphanedState(t *testing.T) {
	m := NewManager()
	// Use a real local runtime to get a live handle for the recovery test.
	rt := runtime.NewLocalRuntime()
	// spawn "sleep 1" — still running so we can attach a recovery handle
	handle, err := rt.Spawn(context.Background(), runtime.SpawnConfig{Cmd: []string{"sleep", "1"}})
	if err != nil {
		t.Skipf("skip: cannot spawn sleep: %v", err)
	}
	defer handle.Kill()

	recovered := m.Recover([]runtime.ProcessHandle{handle}, "local")
	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered session, got %d", len(recovered))
	}
	s := recovered[0]
	if s.State != StateOrphaned {
		t.Fatalf("expected StateOrphaned, got %q", s.State)
	}
	if s.Handle != handle {
		t.Fatal("expected recovered session to have the provided handle")
	}
	if s.RuntimeName != "local" {
		t.Fatalf("expected RuntimeName 'local', got %q", s.RuntimeName)
	}
	// Recovered sessions must be reachable via Get.
	if m.Get(s.ID) == nil {
		t.Fatal("recovered session not findable by ID")
	}
}

// --- Replay buffer default size ---

func TestNewSession_ReplayBufferDefaultSize(t *testing.T) {
	s := NewSession("", "claude", "local")
	// Write 1MiB to verify the default buffer is large enough.
	data := make([]byte, 1<<20)
	n, err := s.Replay.Write(data)
	if err != nil {
		t.Fatalf("write to replay buffer: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes written, got %d", len(data), n)
	}
}

// --- State string values ---

// TestStateConstants ensures the string values of states match what the JSON API
// will return — these values are part of the public API contract.
func TestStateConstants(t *testing.T) {
	cases := []struct {
		state    State
		expected string
	}{
		{StatePending, "pending"},
		{StateRunning, "running"},
		{StateCompleted, "completed"},
		{StateFailed, "failed"},
		{StateOrphaned, "orphaned"},
	}
	for _, tc := range cases {
		if string(tc.state) != tc.expected {
			t.Errorf("State %q: expected string value %q, got %q", tc.state, tc.expected, string(tc.state))
		}
	}
}

// --- Timing ---

func TestSession_EndedAt_SetOnCompletion(t *testing.T) {
	before := time.Now()
	s := NewSession("", "claude", "local")
	s.SetRunning(nil)
	s.SetCompleted(0)
	after := time.Now()

	if s.EndedAt == nil {
		t.Fatal("EndedAt must be set after SetCompleted")
	}
	if s.EndedAt.Before(before) || s.EndedAt.After(after) {
		t.Fatalf("EndedAt %v is outside expected range [%v, %v]", s.EndedAt, before, after)
	}
}

// --- Max sessions limit ---

func TestManager_MaxSessions_EnforcedOnAdd(t *testing.T) {
	m := NewManager()
	m.SetMaxSessions(3)

	for i := 0; i < 3; i++ {
		if err := m.Add(NewSession("", "claude", "local")); err != nil {
			t.Fatalf("Add %d failed unexpectedly: %v", i, err)
		}
	}

	// 4th session must be rejected.
	err := m.Add(NewSession("", "claude", "local"))
	if err != ErrMaxSessions {
		t.Fatalf("expected ErrMaxSessions, got %v", err)
	}
	if len(m.List()) != 3 {
		t.Fatalf("expected 3 sessions after rejected add, got %d", len(m.List()))
	}
}

func TestManager_MaxSessions_RemoveFreesSlot(t *testing.T) {
	m := NewManager()
	m.SetMaxSessions(2)

	s1 := NewSession("", "claude", "local")
	s2 := NewSession("", "claude", "local")
	_ = m.Add(s1)
	_ = m.Add(s2)

	// At limit — new add fails.
	if err := m.Add(NewSession("", "claude", "local")); err != ErrMaxSessions {
		t.Fatalf("expected ErrMaxSessions at limit, got %v", err)
	}

	// Remove one — new add succeeds.
	m.Remove(s1.ID)
	if err := m.Add(NewSession("", "claude", "local")); err != nil {
		t.Fatalf("Add after Remove should succeed, got %v", err)
	}
}

func TestManager_MaxSessions_ZeroMeansUnlimited(t *testing.T) {
	m := NewManager()
	m.SetMaxSessions(0) // explicit unlimited

	for i := 0; i < 50; i++ {
		if err := m.Add(NewSession("", "claude", "local")); err != nil {
			t.Fatalf("Add %d failed with unlimited: %v", i, err)
		}
	}
}

// --- Create 5, delete 3, verify 2 remain ---

func TestManager_CreateFiveDeleteThreeVerifyTwo(t *testing.T) {
	m := NewManager()
	sessions := make([]*Session, 5)
	for i := range sessions {
		s := NewSession("", "claude", "local")
		if err := m.Add(s); err != nil {
			t.Fatalf("Add session %d: %v", i, err)
		}
		// Write some data so the replay buffer is initialized.
		s.Replay.Write([]byte("hello from session " + s.ID))
		sessions[i] = s
	}

	if len(m.List()) != 5 {
		t.Fatalf("expected 5 sessions, got %d", len(m.List()))
	}

	// Delete sessions 0, 1, 2 — simulate what handleDeleteSession does.
	for _, s := range sessions[:3] {
		_ = s.Kill()
		s.Replay.Close()
		s.SetCompleted(-1)
		m.Remove(s.ID)
	}

	// Verify 2 remain.
	remaining := m.List()
	if len(remaining) != 2 {
		t.Fatalf("expected 2 remaining sessions, got %d", len(remaining))
	}

	// Verify the deleted sessions are gone.
	for _, s := range sessions[:3] {
		if m.Get(s.ID) != nil {
			t.Fatalf("deleted session %s still reachable", s.ID)
		}
	}

	// Verify the surviving sessions are still accessible.
	for _, s := range sessions[3:] {
		got := m.Get(s.ID)
		if got == nil {
			t.Fatalf("surviving session %s not found", s.ID)
		}
		if got.State != StatePending {
			t.Fatalf("surviving session state: expected %q, got %q", StatePending, got.State)
		}
	}
}

// --- Replay buffer closure on delete ---

func TestManager_DeleteClosesReplayBuffer(t *testing.T) {
	m := NewManager()
	sessions := make([]*Session, 3)
	for i := range sessions {
		s := NewSession("", "claude", "local")
		_ = m.Add(s)
		s.Replay.Write([]byte("data"))
		sessions[i] = s
	}

	// Delete session 0 — replay buffer must be marked done.
	target := sessions[0]
	_ = target.Kill()
	target.Replay.Close()
	target.SetCompleted(-1)
	m.Remove(target.ID)

	if !target.Replay.IsDone() {
		t.Fatal("replay buffer should be closed (done) after delete")
	}

	// Surviving sessions must still have open replay buffers.
	for _, s := range sessions[1:] {
		if s.Replay.IsDone() {
			t.Fatalf("surviving session %s has closed replay buffer", s.ID)
		}
	}
}

// --- ShutdownAll ---

func TestManager_ShutdownAll_ClosesAllReplayBuffers(t *testing.T) {
	m := NewManager()
	sessions := make([]*Session, 4)
	for i := range sessions {
		s := NewSession("", "claude", "local")
		_ = m.Add(s)
		s.Replay.Write([]byte("output data"))
		sessions[i] = s
	}

	m.ShutdownAll()

	// Registry must be empty.
	if len(m.List()) != 0 {
		t.Fatalf("expected 0 sessions after ShutdownAll, got %d", len(m.List()))
	}

	// All replay buffers must be closed.
	for i, s := range sessions {
		if !s.Replay.IsDone() {
			t.Fatalf("session %d replay buffer not closed after ShutdownAll", i)
		}
	}

	// All sessions must be in a terminal state.
	for i, s := range sessions {
		if s.State != StateFailed {
			t.Fatalf("session %d state: expected %q, got %q", i, StateFailed, s.State)
		}
	}
}
