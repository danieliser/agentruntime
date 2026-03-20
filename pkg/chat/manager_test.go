package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// --- Fakes ---

// fakeVolumeManager records volume create/remove calls.
type fakeVolumeManager struct {
	mu       sync.Mutex
	created  map[string]map[string]string // name → labels
	removed  []string
	createErr error
	removeErr error
}

func newFakeVolumeManager() *fakeVolumeManager {
	return &fakeVolumeManager{created: make(map[string]map[string]string)}
}

func (f *fakeVolumeManager) CreateVolume(_ context.Context, name string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	cp := make(map[string]string, len(labels))
	for k, v := range labels {
		cp[k] = v
	}
	f.created[name] = cp
	return nil
}

func (f *fakeVolumeManager) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, name)
	return nil
}

// fakeSpawner records spawn calls and returns controllable sessions.
type fakeSpawner struct {
	mu       sync.Mutex
	calls    []apischema.SessionRequest
	sessions []*session.Session // pre-built sessions to return in order
	idx      int
	err      error
}

func newFakeSpawner(sessions ...*session.Session) *fakeSpawner {
	return &fakeSpawner{sessions: sessions}
}

func (f *fakeSpawner) SpawnSession(_ context.Context, req apischema.SessionRequest) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.idx >= len(f.sessions) {
		return nil, fmt.Errorf("fakeSpawner: no more sessions (called %d times)", f.idx+1)
	}
	s := f.sessions[f.idx]
	f.idx++
	return s, nil
}

func (f *fakeSpawner) lastRequest() apischema.SessionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeSpawner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeHandle implements runtime.ProcessHandle for testing stdin injection.
type fakeHandle struct {
	stdinBuf *fakeWriteCloser
}

func newFakeHandle() *fakeHandle {
	return &fakeHandle{stdinBuf: &fakeWriteCloser{}}
}

func (h *fakeHandle) Stdin() io.WriteCloser              { return h.stdinBuf }
func (h *fakeHandle) Stdout() io.ReadCloser              { return nil }
func (h *fakeHandle) Stderr() io.ReadCloser              { return nil }
func (h *fakeHandle) Wait() <-chan runtime.ExitResult     { return make(chan runtime.ExitResult) }
func (h *fakeHandle) Kill() error                         { return nil }
func (h *fakeHandle) PID() int                            { return 1234 }
func (h *fakeHandle) RecoveryInfo() *runtime.RecoveryInfo { return nil }

// fakeSteerableHandle implements runtime.SteerableHandle.
type fakeSteerableHandle struct {
	fakeHandle
	mu       sync.Mutex
	prompts  []string
}

func newFakeSteerableHandle() *fakeSteerableHandle {
	return &fakeSteerableHandle{fakeHandle: *newFakeHandle()}
}

func (h *fakeSteerableHandle) SendPrompt(content string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompts = append(h.prompts, content)
	return nil
}
func (h *fakeSteerableHandle) SendInterrupt() error                          { return nil }
func (h *fakeSteerableHandle) SendSteer(string) error                        { return nil }
func (h *fakeSteerableHandle) SendContext(string, string) error              { return nil }
func (h *fakeSteerableHandle) SendMention(string, int, int) error            { return nil }

// fakeWriteCloser captures written bytes.
type fakeWriteCloser struct {
	mu   sync.Mutex
	data []byte
}

func (w *fakeWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *fakeWriteCloser) Close() error { return nil }

func (w *fakeWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.data)
}

// --- Helpers ---

func newTestManager(t *testing.T, vols *fakeVolumeManager, spawner *fakeSpawner) (*Manager, *session.Manager) {
	t.Helper()
	reg, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sessMgr := session.NewManager()
	mgr := NewManager(reg, sessMgr, nil, "docker", vols, spawner)
	return mgr, sessMgr
}

func makeSession(id string, tags map[string]string) *session.Session {
	return session.NewSessionWithID(id, "", "claude", "docker", tags)
}

// --- Tests ---

func TestCreateChat_Docker_CreatesVolume(t *testing.T) {
	vols := newFakeVolumeManager()
	mgr, _ := newTestManager(t, vols, newFakeSpawner())

	rec, err := mgr.CreateChat("web-ui", ChatConfig{Agent: "claude", Runtime: "docker"})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	if rec.State != ChatStateCreated {
		t.Errorf("state = %q, want created", rec.State)
	}
	if rec.VolumeName != "agentruntime-chat-web-ui" {
		t.Errorf("volume = %q, want agentruntime-chat-web-ui", rec.VolumeName)
	}
	if labels, ok := vols.created["agentruntime-chat-web-ui"]; !ok {
		t.Fatal("volume not created")
	} else if labels["agentruntime.chat_name"] != "web-ui" {
		t.Errorf("label = %q, want web-ui", labels["agentruntime.chat_name"])
	}
}

func TestCreateChat_Local_NoVolume(t *testing.T) {
	vols := newFakeVolumeManager()
	mgr, _ := newTestManager(t, vols, newFakeSpawner())
	mgr.defaultRuntime = "local"

	rec, err := mgr.CreateChat("local-chat", ChatConfig{Agent: "claude", Runtime: "local"})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if rec.VolumeName != "" {
		t.Errorf("volume = %q, want empty for local", rec.VolumeName)
	}
	if len(vols.created) != 0 {
		t.Errorf("expected no volume creation for local runtime")
	}
}

func TestCreateChat_DuplicateName(t *testing.T) {
	vols := newFakeVolumeManager()
	mgr, _ := newTestManager(t, vols, newFakeSpawner())

	_, err := mgr.CreateChat("dup", ChatConfig{Agent: "claude"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = mgr.CreateChat("dup", ChatConfig{Agent: "claude"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("second create = %v, want ErrAlreadyExists", err)
	}
}

func TestSendMessage_IdleChat_SpawnsSession(t *testing.T) {
	sess := makeSession("sess-1", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	// Create a chat in idle state.
	rec, _ := mgr.CreateChat("test-idle", ChatConfig{Agent: "claude"})
	rec.State = ChatStateIdle
	mgr.registry.Save(rec)

	result, err := mgr.SendMessage("test-idle", "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !result.Spawned {
		t.Error("expected Spawned=true")
	}
	if result.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", result.SessionID)
	}

	// Verify chat is now running.
	loaded, _ := mgr.GetChat("test-idle")
	if loaded.State != ChatStateRunning {
		t.Errorf("state = %q, want running", loaded.State)
	}
	if loaded.CurrentSession != "sess-1" {
		t.Errorf("CurrentSession = %q, want sess-1", loaded.CurrentSession)
	}
	if len(loaded.SessionChain) != 1 || loaded.SessionChain[0] != "sess-1" {
		t.Errorf("SessionChain = %v, want [sess-1]", loaded.SessionChain)
	}
}

func TestSendMessage_CreatedChat_SpawnsSession(t *testing.T) {
	sess := makeSession("sess-1", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	mgr.CreateChat("new-chat", ChatConfig{Agent: "claude"})

	result, err := mgr.SendMessage("new-chat", "first message")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !result.Spawned {
		t.Error("expected Spawned=true for created chat")
	}

	// Verify spawn request.
	req := spawner.lastRequest()
	if req.Prompt != "first message" {
		t.Errorf("prompt = %q, want 'first message'", req.Prompt)
	}
	if req.Tags["chat_name"] != "new-chat" {
		t.Errorf("tag chat_name = %q, want new-chat", req.Tags["chat_name"])
	}
	if !req.Interactive {
		t.Error("expected Interactive=true")
	}
}

func TestSendMessage_RunningChat_InjectsStdin(t *testing.T) {
	handle := newFakeSteerableHandle()
	sess := makeSession("running-sess", nil)
	sess.SetRunning(handle)

	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	// Create chat and set to running.
	mgr.CreateChat("running", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("running")
	rec.State = ChatStateRunning
	rec.CurrentSession = "running-sess"
	rec.SessionChain = []string{"running-sess"}
	mgr.registry.Save(rec)

	result, err := mgr.SendMessage("running", "follow-up")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if result.Spawned {
		t.Error("expected Spawned=false for running chat")
	}
	if result.SessionID != "running-sess" {
		t.Errorf("SessionID = %q, want running-sess", result.SessionID)
	}

	// Verify prompt was sent via steerable handle.
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if len(handle.prompts) != 1 || handle.prompts[0] != "follow-up" {
		t.Errorf("prompts = %v, want [follow-up]", handle.prompts)
	}
}

func TestSendMessage_RunningChat_PendingExists(t *testing.T) {
	handle := newFakeHandle()
	sess := makeSession("busy-sess", nil)
	sess.SetRunning(handle)

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess)

	mgr.CreateChat("busy", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("busy")
	rec.State = ChatStateRunning
	rec.CurrentSession = "busy-sess"
	rec.PendingMessage = "already queued"
	mgr.registry.Save(rec)

	_, err := mgr.SendMessage("busy", "another message")
	if !errors.Is(err, ErrChatBusy) {
		t.Errorf("err = %v, want ErrChatBusy", err)
	}
}

func TestSendMessage_ResumeSetFromClaudeSessionID(t *testing.T) {
	sess := makeSession("sess-2", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	mgr.CreateChat("resume-test", ChatConfig{Agent: "claude", Runtime: "docker"})
	rec, _ := mgr.GetChat("resume-test")
	rec.State = ChatStateIdle
	rec.SessionChain = []string{"sess-1"}
	rec.VolumeName = "agentruntime-chat-resume-test"
	rec.ClaudeSessionIDs = map[string]string{"sess-1": "claude-abc-123"}
	mgr.registry.Save(rec)

	_, err := mgr.SendMessage("resume-test", "continue")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	req := spawner.lastRequest()
	if req.ResumeSession != "claude-abc-123" {
		t.Errorf("ResumeSession = %q, want claude-abc-123", req.ResumeSession)
	}
}

func TestSendMessage_FirstSpawn_NoResume(t *testing.T) {
	sess := makeSession("first-sess", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	mgr.CreateChat("fresh", ChatConfig{Agent: "claude"})

	_, err := mgr.SendMessage("fresh", "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	req := spawner.lastRequest()
	if req.ResumeSession != "" {
		t.Errorf("ResumeSession = %q, want empty for first spawn", req.ResumeSession)
	}
}

func TestWatchSession_ExitTransitionsToIdle(t *testing.T) {
	sess := makeSession("watch-sess", map[string]string{"claude_session_id": "claude-xyz"})
	sess.SetRunning(nil)

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess)

	mgr.CreateChat("watch-test", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("watch-test")
	rec.State = ChatStateRunning
	rec.CurrentSession = "watch-sess"
	rec.SessionChain = []string{"watch-sess"}
	mgr.registry.Save(rec)

	// Start watching, then complete the session.
	mgr.WatchSession("watch-test", "watch-sess")
	time.Sleep(100 * time.Millisecond) // let watcher start

	sess.SetCompleted(0)

	// Wait for watcher to detect exit.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for idle transition")
		default:
		}
		loaded, _ := mgr.GetChat("watch-test")
		if loaded.State == ChatStateIdle {
			if loaded.CurrentSession != "" {
				t.Errorf("CurrentSession = %q, want empty", loaded.CurrentSession)
			}
			// Verify Claude session ID was captured.
			if loaded.ClaudeSessionIDs == nil || loaded.ClaudeSessionIDs["watch-sess"] != "claude-xyz" {
				t.Errorf("ClaudeSessionIDs = %v, want watch-sess→claude-xyz", loaded.ClaudeSessionIDs)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestWatchSession_PendingConsumedOnExit(t *testing.T) {
	sess1 := makeSession("exit-sess", nil)
	sess1.SetRunning(nil)
	sess2 := makeSession("respawn-sess", nil)

	spawner := newFakeSpawner(sess2)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess1)
	sessMgr.Add(sess2)

	mgr.CreateChat("pending-test", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("pending-test")
	rec.State = ChatStateRunning
	rec.CurrentSession = "exit-sess"
	rec.SessionChain = []string{"exit-sess"}
	rec.PendingMessage = "queued message"
	mgr.registry.Save(rec)

	mgr.WatchSession("pending-test", "exit-sess")
	time.Sleep(100 * time.Millisecond)

	sess1.SetCompleted(0)

	// Wait for respawn.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for respawn")
		default:
		}
		loaded, _ := mgr.GetChat("pending-test")
		if loaded.State == ChatStateRunning && loaded.CurrentSession == "respawn-sess" {
			if loaded.PendingMessage != "" {
				t.Errorf("PendingMessage = %q, want empty", loaded.PendingMessage)
			}
			// Verify spawn was called with the pending message.
			req := spawner.lastRequest()
			if req.Prompt != "queued message" {
				t.Errorf("prompt = %q, want 'queued message'", req.Prompt)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestWatchSession_StaleWatcherIgnored(t *testing.T) {
	sess1 := makeSession("old-sess", nil)
	sess1.SetRunning(nil)

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess1)

	mgr.CreateChat("stale-test", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("stale-test")
	rec.State = ChatStateRunning
	rec.CurrentSession = "new-sess" // Different from what we're watching.
	rec.SessionChain = []string{"old-sess", "new-sess"}
	mgr.registry.Save(rec)

	// Start watching the old session.
	mgr.WatchSession("stale-test", "old-sess")
	time.Sleep(100 * time.Millisecond)

	sess1.SetCompleted(0)

	// Wait for watcher to process exit.
	time.Sleep(5 * time.Second)

	// State should NOT have changed — stale watcher should be ignored.
	loaded, _ := mgr.GetChat("stale-test")
	if loaded.State != ChatStateRunning {
		t.Errorf("state = %q, want running (stale watcher should be ignored)", loaded.State)
	}
	if loaded.CurrentSession != "new-sess" {
		t.Errorf("CurrentSession = %q, want new-sess", loaded.CurrentSession)
	}
}

func TestWatchSession_ResultEventTransitionsToIdle(t *testing.T) {
	handle := newFakeSteerableHandle()
	sess := makeSession("result-sess", map[string]string{"claude_session_id": "claude-res"})
	sess.SetRunning(handle)

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess)

	mgr.CreateChat("result-test", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("result-test")
	rec.State = ChatStateRunning
	rec.CurrentSession = "result-sess"
	rec.SessionChain = []string{"result-sess"}
	mgr.registry.Save(rec)

	// Start the watcher.
	mgr.WatchSession("result-test", "result-sess")
	time.Sleep(100 * time.Millisecond)

	// Fire a result event (turn completed).
	sess.NotifyResult()

	// Wait for idle transition.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for idle transition on result event")
		default:
		}
		loaded, _ := mgr.GetChat("result-test")
		if loaded.State == ChatStateIdle {
			if loaded.CurrentSession != "" {
				t.Errorf("CurrentSession = %q, want empty", loaded.CurrentSession)
			}
			// Verify Claude session ID was captured.
			if loaded.ClaudeSessionIDs == nil || loaded.ClaudeSessionIDs["result-sess"] != "claude-res" {
				t.Errorf("ClaudeSessionIDs = %v, want result-sess→claude-res", loaded.ClaudeSessionIDs)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestWatchSession_ResultWithPending_RespawnsAndAcceptsNext(t *testing.T) {
	// When a result event fires with a PendingMessage set, the watcher
	// should consume it by spawning a fresh session (not re-injecting stdin).
	handle := newFakeSteerableHandle()
	sess1 := makeSession("allow-sess", nil)
	sess1.SetRunning(handle)
	sess2 := makeSession("respawn-sess", nil)

	spawner := newFakeSpawner(sess2)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess1)
	sessMgr.Add(sess2)

	mgr.CreateChat("allow-test", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("allow-test")
	rec.State = ChatStateRunning
	rec.CurrentSession = "allow-sess"
	rec.SessionChain = []string{"allow-sess"}
	mgr.registry.Save(rec)

	// Inject a follow-up via stdin — sets PendingMessage.
	_, err := mgr.SendMessage("allow-test", "first follow-up")
	if err != nil {
		t.Fatalf("first SendMessage: %v", err)
	}
	rec, _ = mgr.GetChat("allow-test")
	if rec.PendingMessage != "first follow-up" {
		t.Fatalf("PendingMessage = %q, want 'first follow-up'", rec.PendingMessage)
	}

	// Start watcher and fire result event.
	mgr.WatchSession("allow-test", "allow-sess")
	time.Sleep(100 * time.Millisecond)
	sess1.NotifyResult()

	// Wait for respawn with the pending message.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for respawn after result event")
		default:
		}
		loaded, _ := mgr.GetChat("allow-test")
		if loaded.CurrentSession == "respawn-sess" && loaded.PendingMessage == "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify spawn was called with the pending message.
	req := spawner.lastRequest()
	if req.Prompt != "first follow-up" {
		t.Errorf("prompt = %q, want 'first follow-up'", req.Prompt)
	}
}

func TestMultiMessageCycle_IdleBetweenTurns(t *testing.T) {
	// Full cycle: message 1 → result → idle → message 2 → result → idle → message 3.
	// This is the primary Docker batch chat pattern.
	sess1 := makeSession("sess-1", map[string]string{"claude_session_id": "claude-1"})
	sess1.SetRunning(newFakeSteerableHandle())
	sess2 := makeSession("sess-2", map[string]string{"claude_session_id": "claude-2"})
	sess2.SetRunning(newFakeSteerableHandle())
	sess3 := makeSession("sess-3", nil)
	sess3.SetRunning(newFakeSteerableHandle())

	spawner := newFakeSpawner(sess1, sess2, sess3)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess1)
	sessMgr.Add(sess2)
	sessMgr.Add(sess3)

	mgr.CreateChat("cycle", ChatConfig{Agent: "claude", Runtime: "docker"})

	// --- Message 1 ---
	res, err := mgr.SendMessage("cycle", "Hi")
	if err != nil {
		t.Fatalf("msg1: %v", err)
	}
	if res.SessionID != "sess-1" || !res.Spawned {
		t.Fatalf("msg1: unexpected result %+v", res)
	}

	// Let the watcher goroutine start before firing the result event.
	time.Sleep(100 * time.Millisecond)
	sess1.NotifyResult()
	waitForState(t, mgr, "cycle", ChatStateIdle, 5*time.Second)

	// Verify resume is wired.
	rec, _ := mgr.GetChat("cycle")
	if rec.ClaudeSessionIDs["sess-1"] != "claude-1" {
		t.Errorf("msg1: ClaudeSessionIDs = %v", rec.ClaudeSessionIDs)
	}

	// --- Message 2 ---
	res, err = mgr.SendMessage("cycle", "Bye")
	if err != nil {
		t.Fatalf("msg2: %v", err)
	}
	if res.SessionID != "sess-2" || !res.Spawned {
		t.Fatalf("msg2: unexpected result %+v", res)
	}

	// Verify resume was passed.
	req2 := spawner.calls[1]
	if req2.ResumeSession != "claude-1" {
		t.Errorf("msg2: ResumeSession = %q, want claude-1", req2.ResumeSession)
	}

	time.Sleep(100 * time.Millisecond)
	sess2.NotifyResult()
	waitForState(t, mgr, "cycle", ChatStateIdle, 5*time.Second)

	// --- Message 3 ---
	res, err = mgr.SendMessage("cycle", "Again")
	if err != nil {
		t.Fatalf("msg3: %v", err)
	}
	if res.SessionID != "sess-3" || !res.Spawned {
		t.Fatalf("msg3: unexpected result %+v", res)
	}

	req3 := spawner.calls[2]
	if req3.ResumeSession != "claude-2" {
		t.Errorf("msg3: ResumeSession = %q, want claude-2", req3.ResumeSession)
	}

	// 3 sessions spawned, no 429 errors.
	if spawner.callCount() != 3 {
		t.Errorf("spawn count = %d, want 3", spawner.callCount())
	}
}


func TestDeleteChat_KillsRunningSession(t *testing.T) {
	handle := newFakeHandle()
	sess := makeSession("kill-sess", nil)
	sess.SetRunning(handle)

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess)

	mgr.CreateChat("delete-running", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("delete-running")
	rec.State = ChatStateRunning
	rec.CurrentSession = "kill-sess"
	mgr.registry.Save(rec)

	err := mgr.DeleteChat("delete-running", false)
	if err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	// Verify record is deleted.
	_, err = mgr.GetChat("delete-running")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v, want ErrNotFound", err)
	}
}

func TestDeleteChat_RemoveVolume(t *testing.T) {
	vols := newFakeVolumeManager()
	mgr, _ := newTestManager(t, vols, newFakeSpawner())

	mgr.CreateChat("vol-delete", ChatConfig{Agent: "claude", Runtime: "docker"})

	err := mgr.DeleteChat("vol-delete", true)
	if err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	if len(vols.removed) != 1 || vols.removed[0] != "agentruntime-chat-vol-delete" {
		t.Errorf("removed = %v, want [agentruntime-chat-vol-delete]", vols.removed)
	}
}

func TestClaudeSessionIDTracking_RoundTrip(t *testing.T) {
	// 1. Create chat.
	sess1 := makeSession("sess-a", map[string]string{"claude_session_id": "claude-111"})
	sess1.SetRunning(nil)
	sess2 := makeSession("sess-b", nil)

	spawner := newFakeSpawner(sess1, sess2)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess1)
	sessMgr.Add(sess2)

	mgr.CreateChat("id-track", ChatConfig{Agent: "claude", Runtime: "docker"})

	// 2. Send first message — spawns sess-a.
	_, err := mgr.SendMessage("id-track", "first")
	if err != nil {
		t.Fatalf("first send: %v", err)
	}

	// 3. Watch session exit — captures claude_session_id.
	mgr.WatchSession("id-track", "sess-a")
	time.Sleep(100 * time.Millisecond)
	sess1.SetCompleted(0)

	// Wait for idle transition.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for idle")
		default:
		}
		loaded, _ := mgr.GetChat("id-track")
		if loaded.State == ChatStateIdle {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 4. Send second message — should resume with captured ID.
	_, err = mgr.SendMessage("id-track", "second")
	if err != nil {
		t.Fatalf("second send: %v", err)
	}

	req := spawner.lastRequest()
	if req.ResumeSession != "claude-111" {
		t.Errorf("ResumeSession = %q, want claude-111", req.ResumeSession)
	}
}

func TestChatLock_DifferentNames(t *testing.T) {
	mgr, _ := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())

	lock1 := mgr.chatLock("a")
	lock2 := mgr.chatLock("b")
	lock1Again := mgr.chatLock("a")

	if lock1 == lock2 {
		t.Error("different names should return different locks")
	}
	if lock1 != lock1Again {
		t.Error("same name should return same lock")
	}
}

func TestSendMessage_DockerMounts(t *testing.T) {
	sess := makeSession("mount-sess", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	mgr.CreateChat("mount-test", ChatConfig{Agent: "claude", Runtime: "docker"})

	_, err := mgr.SendMessage("mount-test", "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	req := spawner.lastRequest()
	if !req.PersistSession {
		t.Error("expected PersistSession=true for docker")
	}
	if len(req.Mounts) != 1 {
		t.Fatalf("mounts = %d, want 1", len(req.Mounts))
	}
	m := req.Mounts[0]
	if m.Host != "agentruntime-chat-mount-test" {
		t.Errorf("mount host = %q, want agentruntime-chat-mount-test", m.Host)
	}
	if m.Container != "/home/agent/.claude/projects" {
		t.Errorf("mount container = %q", m.Container)
	}
	if m.Type != "volume" || m.Mode != "rw" {
		t.Errorf("mount type=%q mode=%q", m.Type, m.Mode)
	}
}

func TestSendMessage_MaxTurns(t *testing.T) {
	sess := makeSession("turns-sess", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	mgr.CreateChat("turns-test", ChatConfig{Agent: "claude", MaxTurns: 5})

	_, err := mgr.SendMessage("turns-test", "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	req := spawner.lastRequest()
	if req.Claude == nil || req.Claude.MaxTurns != 5 {
		t.Errorf("MaxTurns = %v, want 5", req.Claude)
	}
}

func TestSendMessage_EffortTag(t *testing.T) {
	sess := makeSession("effort-sess", nil)
	spawner := newFakeSpawner(sess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess)

	mgr.CreateChat("effort-test", ChatConfig{Agent: "claude", Effort: "high"})

	_, err := mgr.SendMessage("effort-test", "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	req := spawner.lastRequest()
	if req.Tags["effort"] != "high" {
		t.Errorf("effort tag = %q, want high", req.Tags["effort"])
	}
}

func TestRespawnAfterMissing_StartsWatcher(t *testing.T) {
	// When a running chat's session disappears and respawnAfterMissing fires,
	// the new session must be watched for exit. Otherwise the chat stays
	// running forever after the respawned session completes.
	sess1 := makeSession("orig-sess", nil)
	sess1.SetRunning(newFakeHandle())
	sess2 := makeSession("respawn-sess", nil)
	sess2.SetRunning(nil)

	spawner := newFakeSpawner(sess2)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(sess1)
	sessMgr.Add(sess2)

	mgr.CreateChat("respawn-watch", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("respawn-watch")
	rec.State = ChatStateRunning
	rec.CurrentSession = "orig-sess"
	rec.SessionChain = []string{"orig-sess"}
	mgr.registry.Save(rec)

	// Remove the original session from the manager so SendMessage triggers
	// respawnAfterMissing.
	sessMgr.Remove("orig-sess")

	_, err := mgr.SendMessage("respawn-watch", "trigger respawn")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Verify the chat is now running with the respawned session.
	loaded, _ := mgr.GetChat("respawn-watch")
	if loaded.CurrentSession != "respawn-sess" {
		t.Fatalf("CurrentSession = %q, want respawn-sess", loaded.CurrentSession)
	}

	// Now complete the respawned session — the watcher should detect it
	// and transition the chat to idle.
	sess2.SetCompleted(0)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout: watcher never detected respawned session exit")
		default:
		}
		loaded, _ = mgr.GetChat("respawn-watch")
		if loaded.State == ChatStateIdle {
			return // success — watcher is working
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestWatchSession_NilHandle_TreatedAsExit(t *testing.T) {
	// If a session exists in running state but Handle is nil, the watcher
	// should treat it as an exit rather than polling forever.
	sess := makeSession("nil-handle", nil)
	sess.SetRunning(nil) // state=running, handle=nil

	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), newFakeSpawner())
	sessMgr.Add(sess)

	mgr.CreateChat("nil-handle-test", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("nil-handle-test")
	rec.State = ChatStateRunning
	rec.CurrentSession = "nil-handle"
	rec.SessionChain = []string{"nil-handle"}
	mgr.registry.Save(rec)

	mgr.WatchSession("nil-handle-test", "nil-handle")

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout: watcher never detected nil handle")
		default:
		}
		loaded, _ := mgr.GetChat("nil-handle-test")
		if loaded.State == ChatStateIdle {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// failingSteerableHandle returns an error from SendPrompt.
type failingSteerableHandle struct {
	fakeHandle
}

func (h *failingSteerableHandle) SendPrompt(string) error   { return fmt.Errorf("ws disconnected") }
func (h *failingSteerableHandle) SendInterrupt() error       { return nil }
func (h *failingSteerableHandle) SendSteer(string) error     { return nil }
func (h *failingSteerableHandle) SendContext(string, string) error  { return nil }
func (h *failingSteerableHandle) SendMention(string, int, int) error { return nil }

func TestSendMessage_BrokenHandle_Respawns(t *testing.T) {
	// When injectStdin fails (broken WS, dead handle), SendMessage should
	// respawn instead of returning a 500 error.
	brokenSess := makeSession("broken-sess", nil)
	brokenSess.SetRunning(&failingSteerableHandle{fakeHandle: *newFakeHandle()})

	freshSess := makeSession("fresh-sess", nil)

	spawner := newFakeSpawner(freshSess)
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner)
	sessMgr.Add(brokenSess)
	sessMgr.Add(freshSess)

	mgr.CreateChat("broken-test", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("broken-test")
	rec.State = ChatStateRunning
	rec.CurrentSession = "broken-sess"
	rec.SessionChain = []string{"broken-sess"}
	mgr.registry.Save(rec)

	result, err := mgr.SendMessage("broken-test", "hello after crash")
	if err != nil {
		t.Fatalf("SendMessage should respawn, got error: %v", err)
	}
	if !result.Spawned {
		t.Error("expected Spawned=true from respawn")
	}
	if result.SessionID != "fresh-sess" {
		t.Errorf("SessionID = %q, want fresh-sess", result.SessionID)
	}
}
