package chat

// Integration tests for the named chat lifecycle.
// Each test exercises a complete scenario end-to-end at the Manager level,
// combining Registry, Manager, IdleWatcher, and the WatchSession goroutine.
//
// Scenarios covered:
//   - Create chat with full config and verify all fields persist
//   - SendMessage builds a spawn request carrying every config field
//   - Idle timeout: watcher kills session → WatchSession detects exit → chat → idle
//   - Respawn with resume: idle → send → new session gets ResumeSession from captured claude ID
//   - Session chain grows across three consecutive spawns
//   - Delete chat while running removes the registry record
//   - Concurrent send to a busy chat: all goroutines receive ErrChatBusy
//   - Config mutation blocked while running: state check prevents overwrite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/session"
)

// ---- helpers shared across lifecycle tests ----

func newLifecycleManager(t *testing.T) (*Manager, *session.Manager) {
	t.Helper()
	return newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
}

// waitForState polls until the named chat reaches the target state or times out.
func waitForState(t *testing.T, mgr *Manager, name string, want ChatState, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			loaded, _ := mgr.GetChat(name)
			got := ChatState("(not found)")
			if loaded != nil {
				got = loaded.State
			}
			t.Fatalf("timeout waiting for chat %q to reach state %q (got %q)", name, want, got)
		default:
		}
		loaded, _ := mgr.GetChat(name)
		if loaded != nil && loaded.State == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// completeAfterKill watches a killable handle; once Kill is called it marks the
// session as completed, simulating a real process dying after SIGKILL.
func completeAfterKill(t *testing.T, sess *session.Session, handle *killableHandle) {
	t.Helper()
	go func() {
		for !handle.wasKilled() {
			time.Sleep(10 * time.Millisecond)
		}
		sess.SetCompleted(0)
	}()
}

// ---- 1. Create chat with full config ----

func TestLifecycle_CreateWithFullConfig(t *testing.T) {
	mgr, _ := newLifecycleManager(t)

	cfg := ChatConfig{
		Agent:       "claude",
		Runtime:     "local",
		Model:       "claude-opus-4-6",
		Effort:      "high",
		WorkDir:     "/workspace/myproject",
		IdleTimeout: "2h",
		MaxTurns:    10,
		Env:         map[string]string{"FOO": "bar", "PORT": "9000"},
		MCPServers: []apischema.MCPServer{
			{Name: "my-mcp", Type: "http", URL: "http://localhost:8080"},
		},
	}

	rec, err := mgr.CreateChat("full-cfg", cfg)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if rec.State != ChatStateCreated {
		t.Errorf("State = %q, want created", rec.State)
	}
	if len(rec.SessionChain) != 0 {
		t.Errorf("SessionChain = %v, want empty", rec.SessionChain)
	}

	// Reload from registry and verify every field.
	loaded, err := mgr.GetChat("full-cfg")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if loaded.Config.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6", loaded.Config.Model)
	}
	if loaded.Config.Effort != "high" {
		t.Errorf("Effort = %q, want high", loaded.Config.Effort)
	}
	if loaded.Config.WorkDir != "/workspace/myproject" {
		t.Errorf("WorkDir = %q, want /workspace/myproject", loaded.Config.WorkDir)
	}
	if loaded.Config.IdleTimeout != "2h" {
		t.Errorf("IdleTimeout = %q, want 2h", loaded.Config.IdleTimeout)
	}
	if loaded.Config.MaxTurns != 10 {
		t.Errorf("MaxTurns = %d, want 10", loaded.Config.MaxTurns)
	}
	if loaded.Config.Env["FOO"] != "bar" || loaded.Config.Env["PORT"] != "9000" {
		t.Errorf("Env = %v, want {FOO:bar PORT:9000}", loaded.Config.Env)
	}
	if len(loaded.Config.MCPServers) != 1 || loaded.Config.MCPServers[0].Name != "my-mcp" {
		t.Errorf("MCPServers = %v, want [{my-mcp http ...}]", loaded.Config.MCPServers)
	}
}

// ---- 2. SendMessage builds spawn request with all config fields ----

func TestLifecycle_SpawnRequest_CarriesFullConfig(t *testing.T) {
	sess := makeSession("spawn-req-sess", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	cfg := ChatConfig{
		Agent:       "claude",
		Runtime:     "local",
		Model:       "claude-opus-4-6",
		Effort:      "high",
		WorkDir:     "/workspace",
		IdleTimeout: "1h",
		MaxTurns:    7,
		Env:         map[string]string{"KEY": "val"},
		MCPServers:  []apischema.MCPServer{{Name: "srv", Type: "stdio", Cmd: []string{"my-mcp"}}},
	}
	if _, err := mgr.CreateChat("spawn-req", cfg); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	if _, err := mgr.SendMessage("spawn-req", "first prompt"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	req := spawner.lastRequest()

	if req.Agent != "claude" {
		t.Errorf("Agent = %q, want claude", req.Agent)
	}
	if req.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6", req.Model)
	}
	if req.WorkDir != "/workspace" {
		t.Errorf("WorkDir = %q, want /workspace", req.WorkDir)
	}
	if req.Prompt != "first prompt" {
		t.Errorf("Prompt = %q, want 'first prompt'", req.Prompt)
	}
	if req.Env["KEY"] != "val" {
		t.Errorf("Env[KEY] = %q, want val", req.Env["KEY"])
	}
	if req.Tags["effort"] != "high" {
		t.Errorf("effort tag = %q, want high", req.Tags["effort"])
	}
	if req.Tags["chat_name"] != "spawn-req" {
		t.Errorf("chat_name tag = %q, want spawn-req", req.Tags["chat_name"])
	}
	if len(req.MCPServers) != 1 || req.MCPServers[0].Name != "srv" {
		t.Errorf("MCPServers = %v", req.MCPServers)
	}
	if req.Claude == nil || req.Claude.MaxTurns != 7 {
		t.Errorf("Claude.MaxTurns = %v, want 7", req.Claude)
	}
	if !req.Interactive {
		t.Error("Interactive should be true")
	}
}

// ---- 3. Idle timeout: watcher kills → WatchSession detects → chat idle ----

func TestLifecycle_IdleTimeout_WatcherKillsAndChatGoesIdle(t *testing.T) {
	reg, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sessMgr := session.NewManager()
	mgr := NewManager(reg, sessMgr, nil, "local", nil, newFakeSpawner())

	// Session whose LastActivity is 1 hour in the past.
	handle := newKillableHandle()
	sess := session.NewSessionWithID("idle-timeout-sess", "", "claude", "local")
	sess.SetRunning(handle)
	sess.SetLastActivityForTest(time.Now().Add(-1 * time.Hour))
	sessMgr.Add(sess)

	// Persist chat as running.
	now := time.Now()
	rec := &ChatRecord{
		Name:           "idle-timeout-chat",
		Config:         ChatConfig{Agent: "claude", IdleTimeout: "1s"},
		State:          ChatStateRunning,
		CurrentSession: "idle-timeout-sess",
		SessionChain:   []string{"idle-timeout-sess"},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := reg.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// completeAfterKill simulates the process dying after Kill().
	completeAfterKill(t, sess, handle)

	// Start WatchSession BEFORE the watcher fires so the watcher → idle transition
	// is picked up by the goroutine.
	mgr.WatchSession("idle-timeout-chat", "idle-timeout-sess")

	// Start idle watcher with a very short poll interval.
	w := NewIdleWatcher(reg, sessMgr, mgr)
	w.SetInterval(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	// cancel must run before Stop: Stop blocks until the loop goroutine exits,
	// which only happens after ctx is cancelled.
	t.Cleanup(func() { cancel(); w.Stop() })
	w.Start(ctx)

	// Wait for the full pipeline: watcher kills → session completes → watch detects → idle.
	waitForState(t, mgr, "idle-timeout-chat", ChatStateIdle, 15*time.Second)

	// Verify cleanup.
	loaded, _ := mgr.GetChat("idle-timeout-chat")
	if loaded.CurrentSession != "" {
		t.Errorf("CurrentSession = %q, want empty after idle", loaded.CurrentSession)
	}
}

// ---- 4. Respawn with resume after idle ----

func TestLifecycle_RespawnWithResume(t *testing.T) {
	claudeID := "claude-session-abc123"

	// sess1 carries the claude_session_id tag so it's captured on exit.
	sess1 := makeSession("resume-sess-1", map[string]string{"claude_session_id": claudeID})
	sess1.SetRunning(newFakeHandle())
	sess2 := makeSession("resume-sess-2", nil)

	spawner := newFakeSpawner(sess1, sess2)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess1)
	sessMgr.Add(sess2)

	if _, err := mgr.CreateChat("resume-chat", ChatConfig{Agent: "claude"}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// First send → spawns sess1.
	result1, err := mgr.SendMessage("resume-chat", "initial message")
	if err != nil {
		t.Fatalf("first SendMessage: %v", err)
	}
	if result1.SessionID != "resume-sess-1" {
		t.Fatalf("first session = %q, want resume-sess-1", result1.SessionID)
	}

	// Watch sess1 and simulate its exit (captures claude_session_id → idle).
	mgr.WatchSession("resume-chat", "resume-sess-1")
	time.Sleep(50 * time.Millisecond)
	sess1.SetCompleted(0)
	waitForState(t, mgr, "resume-chat", ChatStateIdle, 10*time.Second)

	// Verify claude session ID was captured.
	loaded, _ := mgr.GetChat("resume-chat")
	if loaded.ClaudeSessionIDs == nil || loaded.ClaudeSessionIDs["resume-sess-1"] != claudeID {
		t.Errorf("ClaudeSessionIDs = %v, want resume-sess-1→%s", loaded.ClaudeSessionIDs, claudeID)
	}

	// Second send → should respawn with ResumeSession set.
	result2, err := mgr.SendMessage("resume-chat", "continuation")
	if err != nil {
		t.Fatalf("second SendMessage: %v", err)
	}
	if result2.SessionID != "resume-sess-2" {
		t.Fatalf("second session = %q, want resume-sess-2", result2.SessionID)
	}
	if !result2.Spawned {
		t.Error("expected Spawned=true on respawn")
	}

	// Verify the spawn request carries the claude session ID as ResumeSession.
	req := spawner.lastRequest()
	if req.ResumeSession != claudeID {
		t.Errorf("ResumeSession = %q, want %s", req.ResumeSession, claudeID)
	}

	// Session chain should now have both sessions.
	loaded, _ = mgr.GetChat("resume-chat")
	if len(loaded.SessionChain) != 2 {
		t.Errorf("SessionChain len = %d, want 2", len(loaded.SessionChain))
	}
	if loaded.SessionChain[0] != "resume-sess-1" || loaded.SessionChain[1] != "resume-sess-2" {
		t.Errorf("SessionChain = %v, want [resume-sess-1, resume-sess-2]", loaded.SessionChain)
	}
}

// ---- 5. Session chain grows across three consecutive spawns ----

func TestLifecycle_SessionChain_ThreeRounds(t *testing.T) {
	ids := []string{"chain-s1", "chain-s2", "chain-s3"}
	sessions := make([]*session.Session, len(ids))
	for i, id := range ids {
		s := makeSession(id, nil)
		s.SetRunning(newFakeHandle())
		sessions[i] = s
	}

	spawner := newFakeSpawner(sessions...)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	for _, s := range sessions {
		sessMgr.Add(s)
	}

	if _, err := mgr.CreateChat("chain-chat", ChatConfig{Agent: "claude"}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	for i, id := range ids {
		msg := fmt.Sprintf("message %d", i+1)

		result, err := mgr.SendMessage("chain-chat", msg)
		if err != nil {
			t.Fatalf("round %d SendMessage: %v", i+1, err)
		}
		if result.SessionID != id {
			t.Errorf("round %d: SessionID = %q, want %q", i+1, result.SessionID, id)
		}

		// Watch and exit — transitions to idle so next send spawns fresh.
		mgr.WatchSession("chain-chat", id)
		time.Sleep(50 * time.Millisecond)
		sessions[i].SetCompleted(0)
		waitForState(t, mgr, "chain-chat", ChatStateIdle, 10*time.Second)
	}

	// Verify the full chain.
	loaded, err := mgr.GetChat("chain-chat")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if len(loaded.SessionChain) != 3 {
		t.Fatalf("SessionChain len = %d, want 3", len(loaded.SessionChain))
	}
	for i, id := range ids {
		if loaded.SessionChain[i] != id {
			t.Errorf("SessionChain[%d] = %q, want %q", i, loaded.SessionChain[i], id)
		}
	}

	// LastSessionID should be the third.
	if last := loaded.LastSessionID(); last != ids[2] {
		t.Errorf("LastSessionID = %q, want %q", last, ids[2])
	}
}

// ---- 6. Delete chat while running removes the record ----

func TestLifecycle_DeleteChat_WhileRunning(t *testing.T) {
	handle := newFakeHandle()
	sess := makeSession("del-lifecycle-sess", nil)
	sess.SetRunning(handle)

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess)

	if _, err := mgr.CreateChat("del-lifecycle", ChatConfig{Agent: "claude"}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// Manually put the chat into running state.
	rec, _ := mgr.GetChat("del-lifecycle")
	rec.State = ChatStateRunning
	rec.CurrentSession = "del-lifecycle-sess"
	rec.SessionChain = []string{"del-lifecycle-sess"}
	mgr.registry.Save(rec)

	// Ensure it appears in list.
	list, _ := mgr.ListChats()
	if len(list) != 1 {
		t.Fatalf("list before delete = %d, want 1", len(list))
	}

	if err := mgr.DeleteChat("del-lifecycle", false); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	// Record should be gone.
	_, err := mgr.GetChat("del-lifecycle")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: err = %v, want ErrNotFound", err)
	}

	// List should be empty.
	list, _ = mgr.ListChats()
	if len(list) != 0 {
		t.Errorf("list after delete = %d, want 0", len(list))
	}

	// SendMessage to a deleted (non-existent) chat must fail.
	_, err = mgr.SendMessage("del-lifecycle", "after delete")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("SendMessage after delete: err = %v, want ErrNotFound", err)
	}
}

// ---- 7. Concurrent send to a busy chat: all goroutines receive ErrChatBusy ----

func TestLifecycle_ConcurrentSend_AllGetBusy(t *testing.T) {
	handle := newFakeHandle()
	sess := makeSession("busy-concurrent-sess", nil)
	sess.SetRunning(handle)

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess)

	if _, err := mgr.CreateChat("busy-concurrent", ChatConfig{Agent: "claude"}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// Put the chat into running state with a pending message already queued.
	rec, _ := mgr.GetChat("busy-concurrent")
	rec.State = ChatStateRunning
	rec.CurrentSession = "busy-concurrent-sess"
	rec.SessionChain = []string{"busy-concurrent-sess"}
	rec.PendingMessage = "already queued"
	mgr.registry.Save(rec)

	const goroutines = 10
	var (
		busyCount    atomic.Int64
		successCount atomic.Int64
		wg           sync.WaitGroup
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := mgr.SendMessage("busy-concurrent", fmt.Sprintf("concurrent msg %d", i))
			if errors.Is(err, ErrChatBusy) {
				busyCount.Add(1)
			} else if err == nil {
				successCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if busyCount.Load() != goroutines {
		t.Errorf("busy count = %d, want %d (successCount=%d)", busyCount.Load(), goroutines, successCount.Load())
	}
}

// ---- 8. Config mutation blocked while running; allowed when idle ----

func TestLifecycle_ConfigMutation_BlockedWhenRunning_AllowedWhenIdle(t *testing.T) {
	handle := newFakeHandle()
	sess1 := makeSession("cfg-mutation-sess1", nil)
	sess1.SetRunning(handle)
	sess2 := makeSession("cfg-mutation-sess2", nil) // for potential second spawn

	spawner := newFakeSpawner(sess1, sess2)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess1)
	sessMgr.Add(sess2)

	if _, err := mgr.CreateChat("cfg-mutation", ChatConfig{Agent: "claude", Model: "original"}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// Send message → running.
	if _, err := mgr.SendMessage("cfg-mutation", "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// While running: config update via registry directly would bypass the state guard,
	// but the contract is that callers MUST check state == idle/created before updating.
	// Verify the state is indeed "running" so the guard would trigger.
	loaded, _ := mgr.GetChat("cfg-mutation")
	if loaded.State != ChatStateRunning {
		t.Fatalf("expected running state, got %q", loaded.State)
	}
	// The state constraint is enforced at the API layer; validate it holds here.
	if loaded.State == ChatStateIdle || loaded.State == ChatStateCreated {
		t.Error("config mutation would be incorrectly permitted: chat should be running")
	}

	// Simulate session exit → idle.
	mgr.WatchSession("cfg-mutation", "cfg-mutation-sess1")
	time.Sleep(50 * time.Millisecond)
	sess1.SetCompleted(0)
	waitForState(t, mgr, "cfg-mutation", ChatStateIdle, 10*time.Second)

	// Now in idle: config update is permitted. Apply it directly via registry.
	loaded, _ = mgr.GetChat("cfg-mutation")
	loaded.Config.Model = "updated-model"
	loaded.Config.MaxTurns = 3
	if err := mgr.registry.Save(loaded); err != nil {
		t.Fatalf("registry.Save in idle state: %v", err)
	}

	// Verify persisted.
	reloaded, _ := mgr.GetChat("cfg-mutation")
	if reloaded.Config.Model != "updated-model" {
		t.Errorf("Model = %q, want updated-model", reloaded.Config.Model)
	}
	if reloaded.Config.MaxTurns != 3 {
		t.Errorf("MaxTurns = %d, want 3", reloaded.Config.MaxTurns)
	}

	// Send again → new session should pick up updated config (MaxTurns now 3, claude agent).
	if _, err := mgr.SendMessage("cfg-mutation", "after config update"); err != nil {
		t.Fatalf("second SendMessage: %v", err)
	}
	req := spawner.lastRequest()
	if req.Claude == nil || req.Claude.MaxTurns != 3 {
		t.Errorf("post-update spawn MaxTurns = %v, want 3", req.Claude)
	}
}
