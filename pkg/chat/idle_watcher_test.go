package chat

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// killableHandle tracks whether Kill was called.
type killableHandle struct {
	killed atomic.Bool
}

func newKillableHandle() *killableHandle { return &killableHandle{} }

func (h *killableHandle) Stdin() io.WriteCloser              { return nil }
func (h *killableHandle) Stdout() io.ReadCloser              { return nil }
func (h *killableHandle) Stderr() io.ReadCloser              { return nil }
func (h *killableHandle) Wait() <-chan runtime.ExitResult     { return make(chan runtime.ExitResult) }
func (h *killableHandle) PID() int                            { return 9999 }
func (h *killableHandle) RecoveryInfo() *runtime.RecoveryInfo { return nil }

func (h *killableHandle) Kill() error {
	h.killed.Store(true)
	return nil
}

func (h *killableHandle) wasKilled() bool {
	return h.killed.Load()
}

// newTestIdleWatcher creates a watcher wired to a temp registry with a short interval.
func newTestIdleWatcher(t *testing.T, interval time.Duration) (*IdleWatcher, *Registry, *session.Manager) {
	t.Helper()
	reg, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sessMgr := session.NewManager()
	// Manager is needed but not used by the watcher directly for kills.
	mgr := NewManager(reg, sessMgr, nil, "local", nil, nil)
	w := NewIdleWatcher(reg, sessMgr, mgr)
	w.SetInterval(interval)
	return w, reg, sessMgr
}

func saveRunningChat(t *testing.T, reg *Registry, name, sessionID string, idleTimeout string) {
	t.Helper()
	now := time.Now()
	rec := &ChatRecord{
		Name:           name,
		Config:         ChatConfig{Agent: "claude", IdleTimeout: idleTimeout},
		State:          ChatStateRunning,
		CurrentSession: sessionID,
		SessionChain:   []string{sessionID},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := reg.Save(rec); err != nil {
		t.Fatalf("Save chat: %v", err)
	}
}

func TestIdleWatcher_KillsIdleSession(t *testing.T) {
	w, reg, sessMgr := newTestIdleWatcher(t, 50*time.Millisecond)

	handle := newKillableHandle()
	sess := session.NewSessionWithID("idle-sess", "", "claude", "local")
	sess.SetRunning(handle)
	// Set LastActivity far in the past.
	past := time.Now().Add(-1 * time.Hour)
	sess.SetLastActivityForTest(past)
	sessMgr.Add(sess)

	saveRunningChat(t, reg, "idle-chat", "idle-sess", "1s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Wait for the watcher to kill it.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for session kill")
		default:
		}
		if handle.wasKilled() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIdleWatcher_SkipsActiveSession(t *testing.T) {
	w, reg, sessMgr := newTestIdleWatcher(t, 50*time.Millisecond)

	handle := newKillableHandle()
	sess := session.NewSessionWithID("active-sess", "", "claude", "local")
	sess.SetRunning(handle)
	sess.RecordActivity() // Just now — should not be killed.
	sessMgr.Add(sess)

	saveRunningChat(t, reg, "active-chat", "active-sess", "1h")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Let several ticks run.
	time.Sleep(200 * time.Millisecond)
	cancel()
	w.Stop()

	if handle.wasKilled() {
		t.Error("active session should not have been killed")
	}
}

func TestIdleWatcher_SkipsTerminalSession(t *testing.T) {
	w, reg, sessMgr := newTestIdleWatcher(t, 50*time.Millisecond)

	handle := newKillableHandle()
	sess := session.NewSessionWithID("done-sess", "", "claude", "local")
	sess.SetRunning(handle)
	past := time.Now().Add(-1 * time.Hour)
	sess.SetLastActivityForTest(past)
	sess.SetCompleted(0) // Terminal state.
	sessMgr.Add(sess)

	saveRunningChat(t, reg, "done-chat", "done-sess", "1s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()
	w.Stop()

	if handle.wasKilled() {
		t.Error("terminal session should not have been killed")
	}
}

func TestIdleWatcher_SkipsIdleChats(t *testing.T) {
	w, reg, sessMgr := newTestIdleWatcher(t, 50*time.Millisecond)

	handle := newKillableHandle()
	sess := session.NewSessionWithID("idle-state-sess", "", "claude", "local")
	sess.SetRunning(handle)
	past := time.Now().Add(-1 * time.Hour)
	sess.SetLastActivityForTest(past)
	sessMgr.Add(sess)

	// Save as idle state — should not be inspected.
	now := time.Now()
	rec := &ChatRecord{
		Name:           "idle-state-chat",
		Config:         ChatConfig{Agent: "claude", IdleTimeout: "1s"},
		State:          ChatStateIdle,
		CurrentSession: "",
		SessionChain:   []string{},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	reg.Save(rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()
	w.Stop()

	if handle.wasKilled() {
		t.Error("idle-state chat's session should not have been killed")
	}
}

func TestIdleWatcher_CustomTimeout(t *testing.T) {
	w, reg, sessMgr := newTestIdleWatcher(t, 50*time.Millisecond)

	// Session with activity 500ms ago.
	handle := newKillableHandle()
	sess := session.NewSessionWithID("custom-sess", "", "claude", "local")
	sess.SetRunning(handle)
	recent := time.Now().Add(-500 * time.Millisecond)
	sess.SetLastActivityForTest(recent)
	sessMgr.Add(sess)

	// Timeout is 200ms — should be killed (500ms > 200ms).
	saveRunningChat(t, reg, "custom-chat", "custom-sess", "200ms")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for session kill with custom timeout")
		default:
		}
		if handle.wasKilled() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIdleWatcher_Stop(t *testing.T) {
	w, _, _ := newTestIdleWatcher(t, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	// Cancel should cause the loop to exit.
	cancel()

	// Stop should return promptly.
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after context cancellation")
	}
}

// TestIdleWatcher_ConcurrentTicks verifies no data races under concurrent ticks.
func TestIdleWatcher_ConcurrentTicks(t *testing.T) {
	w, reg, sessMgr := newTestIdleWatcher(t, 10*time.Millisecond)

	var handles []*killableHandle
	for i := 0; i < 5; i++ {
		h := newKillableHandle()
		handles = append(handles, h)
		id := "concurrent-" + string(rune('a'+i))
		s := session.NewSessionWithID(id, "", "claude", "local")
		s.SetRunning(h)
		s.RecordActivity()
		sessMgr.Add(s)
		saveRunningChat(t, reg, "cc-"+id, id, "1h")
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	// Run for a bit with concurrent activity updates.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		id := "concurrent-" + string(rune('a'+i))
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				s := sessMgr.Get(sid)
				if s != nil {
					s.RecordActivity()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(id)
	}
	wg.Wait()
	cancel()
	w.Stop()

	// None should have been killed (1h timeout, recent activity).
	for i, h := range handles {
		if h.wasKilled() {
			t.Errorf("handle %d was killed unexpectedly", i)
		}
	}
}
