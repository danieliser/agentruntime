package chat

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// ErrAlreadyExists is returned when creating a chat that already exists.
var ErrAlreadyExists = errors.New("chat already exists")

// ErrChatBusy is returned when a chat already has a pending message queued.
var ErrChatBusy = errors.New("chat busy: pending message slot full")

// VolumeManager abstracts Docker volume operations for testability.
type VolumeManager interface {
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
}

// SessionSpawner abstracts session creation so the manager doesn't depend on
// the full API server. Implementations wire into the existing session creation
// pipeline (session dir prep, runtime spawn, IO attach).
type SessionSpawner interface {
	SpawnSession(ctx context.Context, req apischema.SessionRequest) (*session.Session, error)
}

// SendResult describes the outcome of SendMessage.
type SendResult struct {
	SessionID string    `json:"session_id"`
	Queued    bool      `json:"queued"`
	Spawned   bool      `json:"spawned"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager orchestrates named chat lifecycle: create, send messages,
// watch session exits, and transition state.
type Manager struct {
	registry       *Registry
	sessions       *session.Manager
	runtimes       map[string]runtime.Runtime
	defaultRuntime string
	volumes        VolumeManager
	spawner        SessionSpawner

	mu       sync.Mutex
	chatLocks map[string]*sync.Mutex
}

// NewManager creates a Manager wired to the given dependencies.
func NewManager(
	registry *Registry,
	sessions *session.Manager,
	runtimes map[string]runtime.Runtime,
	defaultRuntime string,
	volumes VolumeManager,
	spawner SessionSpawner,
) *Manager {
	return &Manager{
		registry:       registry,
		sessions:       sessions,
		runtimes:       runtimes,
		defaultRuntime: defaultRuntime,
		volumes:        volumes,
		spawner:        spawner,
		chatLocks:      make(map[string]*sync.Mutex),
	}
}

// SetSpawner sets the session spawner after construction.
// Used to break the circular dependency between api.Server and chat.Manager.
func (m *Manager) SetSpawner(spawner SessionSpawner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawner = spawner
}

// chatLock returns (or lazily creates) the per-chat mutex.
func (m *Manager) chatLock(name string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mu, ok := m.chatLocks[name]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	m.chatLocks[name] = mu
	return mu
}

// CreateChat creates a new named chat with the given config.
// For docker runtime, a named volume is created. Returns ErrAlreadyExists
// if a chat with this name already exists.
func (m *Manager) CreateChat(name string, cfg ChatConfig) (*ChatRecord, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	lock := m.chatLock(name)
	lock.Lock()
	defer lock.Unlock()

	if m.registry.Exists(name) {
		return nil, ErrAlreadyExists
	}

	var volumeName string
	rt := m.effectiveRuntime(cfg.Runtime)
	if rt == "docker" && m.volumes != nil {
		volumeName = "agentruntime-chat-" + name
		labels := map[string]string{"agentruntime.chat_name": name}
		if err := m.volumes.CreateVolume(context.Background(), volumeName, labels); err != nil {
			return nil, fmt.Errorf("create chat volume: %w", err)
		}
	}

	now := time.Now()
	rec := &ChatRecord{
		Name:         name,
		Config:       cfg,
		State:        ChatStateCreated,
		VolumeName:   volumeName,
		SessionChain: []string{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := m.registry.Save(rec); err != nil {
		return nil, fmt.Errorf("save chat record: %w", err)
	}
	return rec, nil
}

// GetChat returns the current ChatRecord.
func (m *Manager) GetChat(name string) (*ChatRecord, error) {
	return m.registry.Load(name)
}

// ListChats returns all chat records.
func (m *Manager) ListChats() ([]*ChatRecord, error) {
	return m.registry.List()
}

// DeleteChat transitions the chat to deleted state.
// If running, kills the current session first. If removeVolume is true and a
// volume exists, it is removed.
func (m *Manager) DeleteChat(name string, removeVolume bool) error {
	lock := m.chatLock(name)
	lock.Lock()
	defer lock.Unlock()

	rec, err := m.registry.Load(name)
	if err != nil {
		return err
	}

	if rec.State == ChatStateRunning && rec.CurrentSession != "" {
		m.killSession(rec.CurrentSession)
	}

	if removeVolume && rec.VolumeName != "" && m.volumes != nil {
		if err := m.volumes.RemoveVolume(context.Background(), rec.VolumeName); err != nil {
			log.Printf("[chat %s] warning: failed to remove volume %s: %v", name, rec.VolumeName, err)
		}
	}

	return m.registry.Delete(name)
}

// SendMessage sends a message to the named chat.
//   - created/idle: spawns a new session with the message as initial prompt.
//   - running + no pending: injects message via stdin to the running session.
//   - running + pending slot taken: returns ErrChatBusy.
func (m *Manager) SendMessage(name, message string) (*SendResult, error) {
	lock := m.chatLock(name)
	lock.Lock()
	defer lock.Unlock()

	rec, err := m.registry.Load(name)
	if err != nil {
		return nil, err
	}

	if rec.State == ChatStateDeleted {
		return nil, ErrNotFound
	}

	now := time.Now()

	switch rec.State {
	case ChatStateCreated, ChatStateIdle:
		sessID, err := m.spawnSession(rec, message)
		if err != nil {
			return nil, fmt.Errorf("spawn session: %w", err)
		}
		rec.State = ChatStateRunning
		rec.CurrentSession = sessID
		rec.SessionChain = append(rec.SessionChain, sessID)
		rec.LastActiveAt = &now
		rec.UpdatedAt = now
		if err := m.registry.Save(rec); err != nil {
			return nil, fmt.Errorf("save after spawn: %w", err)
		}
		// Start watching the session for exit — transitions chat to idle when agent finishes.
		m.WatchSession(rec.Name, sessID)
		return &SendResult{
			SessionID: sessID,
			Spawned:   true,
			CreatedAt: now,
		}, nil

	case ChatStateRunning:
		if rec.PendingMessage != "" {
			// Queue depth is 1 — reject if slot is already taken.
			return nil, ErrChatBusy
		}
		// Inject via stdin to the running session.
		sess := m.sessions.Get(rec.CurrentSession)
		if sess == nil {
			// Session disappeared — transition to idle and re-spawn.
			return m.respawnAfterMissing(rec, message, now)
		}
		if err := m.injectStdin(sess, message); err != nil {
			return nil, fmt.Errorf("inject stdin: %w", err)
		}
		// Mark pending so the next concurrent caller hits ErrChatBusy.
		// Cleared by handleSessionExit when the turn completes.
		rec.PendingMessage = message
		rec.LastActiveAt = &now
		rec.UpdatedAt = now
		if err := m.registry.Save(rec); err != nil {
			return nil, fmt.Errorf("save after stdin: %w", err)
		}
		return &SendResult{
			SessionID: rec.CurrentSession,
			CreatedAt: now,
		}, nil

	default:
		return nil, fmt.Errorf("unexpected chat state: %s", rec.State)
	}
}

// AttachSession spawns an interactive session (no prompt) for the named chat.
// If the chat is already running, returns the current session ID.
// If idle/created, spawns a new interactive session with resume wired.
// The session is tracked by the chat manager for lifecycle and resume support.
func (m *Manager) AttachSession(name string) (*SendResult, error) {
	lock := m.chatLock(name)
	lock.Lock()
	defer lock.Unlock()

	rec, err := m.registry.Load(name)
	if err != nil {
		return nil, err
	}
	if rec.State == ChatStateDeleted {
		return nil, ErrNotFound
	}

	now := time.Now()

	switch rec.State {
	case ChatStateRunning:
		// Already running — return current session.
		return &SendResult{
			SessionID: rec.CurrentSession,
			CreatedAt: now,
		}, nil

	case ChatStateCreated, ChatStateIdle:
		// Spawn interactive session (empty prompt → sidecar interactive mode).
		sessID, err := m.spawnSession(rec, "")
		if err != nil {
			return nil, fmt.Errorf("spawn session: %w", err)
		}
		rec.State = ChatStateRunning
		rec.CurrentSession = sessID
		rec.SessionChain = append(rec.SessionChain, sessID)
		rec.LastActiveAt = &now
		rec.UpdatedAt = now
		if err := m.registry.Save(rec); err != nil {
			return nil, fmt.Errorf("save after spawn: %w", err)
		}
		m.WatchSession(rec.Name, sessID)
		return &SendResult{
			SessionID: sessID,
			Spawned:   true,
			CreatedAt: now,
		}, nil

	default:
		return nil, fmt.Errorf("unexpected chat state: %s", rec.State)
	}
}

// WatchSession registers an exit watcher for a chat-backed session.
// On exit: captures Claude session ID, then respawns if pending or transitions to idle.
func (m *Manager) WatchSession(name, sessionID string) {
	go m.watchSessionLoop(name, sessionID)
}

// watchSessionLoop polls session status and handles exit.
func (m *Manager) watchSessionLoop(name, sessionID string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		sess := m.sessions.Get(sessionID)
		if sess == nil {
			// Session removed from manager — treat as exited.
			m.handleSessionExit(name, sessionID, nil)
			return
		}
		snap := sess.Snapshot()
		if isTerminalState(snap.State) {
			m.handleSessionExit(name, sessionID, &snap)
			return
		}
	}
}

// handleSessionExit processes a session exit for the named chat.
func (m *Manager) handleSessionExit(name, sessionID string, snap *session.Session) {
	lock := m.chatLock(name)
	lock.Lock()
	defer lock.Unlock()

	rec, err := m.registry.Load(name)
	if err != nil {
		log.Printf("[chat %s] watch: failed to load record: %v", name, err)
		return
	}

	// Stale watcher — a newer session has taken over.
	if rec.CurrentSession != sessionID {
		return
	}

	// Capture Claude session ID from tags.
	if snap != nil && snap.Tags != nil {
		if claudeID, ok := snap.Tags["claude_session_id"]; ok && claudeID != "" {
			if rec.ClaudeSessionIDs == nil {
				rec.ClaudeSessionIDs = make(map[string]string)
			}
			rec.ClaudeSessionIDs[sessionID] = claudeID
		}
	}

	now := time.Now()

	// If there's a pending message, consume it and respawn.
	if rec.PendingMessage != "" {
		pending := rec.PendingMessage
		rec.PendingMessage = ""
		rec.UpdatedAt = now
		if err := m.registry.Save(rec); err != nil {
			log.Printf("[chat %s] watch: failed to save before respawn: %v", name, err)
			return
		}

		newID, err := m.spawnSession(rec, pending)
		if err != nil {
			log.Printf("[chat %s] watch: failed to respawn with pending: %v", name, err)
			rec.State = ChatStateIdle
			rec.CurrentSession = ""
			rec.UpdatedAt = time.Now()
			_ = m.registry.Save(rec)
			return
		}

		rec.State = ChatStateRunning
		rec.CurrentSession = newID
		rec.SessionChain = append(rec.SessionChain, newID)
		rec.LastActiveAt = &now
		rec.UpdatedAt = time.Now()
		if err := m.registry.Save(rec); err != nil {
			log.Printf("[chat %s] watch: failed to save after respawn: %v", name, err)
			return
		}

		// Watch the new session.
		go m.watchSessionLoop(name, newID)
		return
	}

	// No pending — transition to idle.
	rec.State = ChatStateIdle
	rec.CurrentSession = ""
	rec.UpdatedAt = now
	if err := m.registry.Save(rec); err != nil {
		log.Printf("[chat %s] watch: failed to save idle transition: %v", name, err)
	}
}

// spawnSession builds a SessionRequest from the chat record and spawns it.
// Returns the new session ID.
func (m *Manager) spawnSession(rec *ChatRecord, message string) (string, error) {
	rt := m.effectiveRuntime(rec.Config.Runtime)
	isDocker := rt == "docker"

	tags := map[string]string{"chat_name": rec.Name}
	if rec.Config.Effort != "" {
		tags["effort"] = rec.Config.Effort
	}

	req := apischema.SessionRequest{
		Agent:        rec.Config.Agent,
		Runtime:      rt,
		Model:        rec.Config.Model,
		Prompt:       message,
		Interactive:  true,
		MCPServers:   rec.Config.MCPServers,
		AutoDiscover: rec.Config.AutoDiscover,
		WorkDir:      rec.Config.WorkDir,
		Env:          rec.Config.Env,
		Tags:         tags,
	}

	if isDocker {
		req.PersistSession = true
	}

	// Set MaxTurns via Claude config if specified.
	if rec.Config.MaxTurns > 0 && rec.Config.Agent == "claude" {
		req.Claude = &apischema.ClaudeConfig{
			MaxTurns: rec.Config.MaxTurns,
		}
	}

	// Wire resume from the last session's Claude session ID.
	if lastID := rec.LastSessionID(); lastID != "" && rec.ClaudeSessionIDs != nil {
		if claudeID, ok := rec.ClaudeSessionIDs[lastID]; ok && claudeID != "" {
			req.ResumeSession = claudeID
		}
	}

	// Mount the chat volume for Docker.
	if isDocker && rec.VolumeName != "" {
		req.Mounts = append(req.Mounts, apischema.Mount{
			Host:      rec.VolumeName,
			Container: "/home/agent/.claude/projects",
			Type:      "volume",
			Mode:      "rw",
		})
	}

	sess, err := m.spawner.SpawnSession(context.Background(), req)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// injectStdin writes a message to the running session's stdin.
// Uses SteerableHandle.SendPrompt if available, otherwise raw stdin write.
func (m *Manager) injectStdin(sess *session.Session, message string) error {
	if sess.Handle == nil {
		return fmt.Errorf("session has no process handle")
	}

	// Prefer the sidecar prompt command for steerable handles.
	if sh, ok := sess.Handle.(runtime.SteerableHandle); ok {
		return sh.SendPrompt(message)
	}

	// Fallback: raw stdin write.
	stdin := sess.Handle.Stdin()
	if stdin == nil {
		return fmt.Errorf("session stdin is closed")
	}
	_, err := fmt.Fprintf(stdin, "%s\n", message)
	return err
}

// killSession terminates a session by ID.
func (m *Manager) killSession(sessionID string) {
	sess := m.sessions.Get(sessionID)
	if sess == nil {
		return
	}
	if err := sess.Kill(); err != nil {
		log.Printf("[chat] failed to kill session %s: %v", sessionID, err)
	}
}

// respawnAfterMissing handles the case where a running chat's session has
// disappeared from the session manager. Transitions to idle and spawns fresh.
func (m *Manager) respawnAfterMissing(rec *ChatRecord, message string, now time.Time) (*SendResult, error) {
	sessID, err := m.spawnSession(rec, message)
	if err != nil {
		return nil, fmt.Errorf("respawn after missing session: %w", err)
	}
	rec.State = ChatStateRunning
	rec.CurrentSession = sessID
	rec.SessionChain = append(rec.SessionChain, sessID)
	rec.LastActiveAt = &now
	rec.UpdatedAt = now
	if err := m.registry.Save(rec); err != nil {
		return nil, fmt.Errorf("save after respawn: %w", err)
	}
	return &SendResult{
		SessionID: sessID,
		Spawned:   true,
		CreatedAt: now,
	}, nil
}

// effectiveRuntime returns the runtime name to use, falling back to the
// manager's default if empty.
func (m *Manager) effectiveRuntime(requested string) string {
	if requested != "" {
		return requested
	}
	return m.defaultRuntime
}

// isTerminalState reports whether a session state is terminal.
func isTerminalState(s session.State) bool {
	switch s {
	case session.StateCompleted, session.StateFailed, session.StateOrphaned:
		return true
	default:
		return false
	}
}
