// Package session manages agent session lifecycle, state tracking, and the
// in-memory session registry.
package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/google/uuid"
)

// State represents the lifecycle state of a session.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateOrphaned  State = "orphaned"
)

// Session represents a running or completed agent process with its associated
// metadata, replay buffer, and process handle.
type Session struct {
	ID          string                `json:"id"`
	TaskID      string                `json:"task_id,omitempty"`
	AgentName   string                `json:"agent_name"`
	RuntimeName string                `json:"runtime_name"`
	SessionDir  string                `json:"session_dir,omitempty"`
	VolumeName  string                `json:"volume_name,omitempty"`
	Tags        map[string]string     `json:"tags,omitempty"`
	State       State                 `json:"state"`
	ExitCode    *int                  `json:"exit_code,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	EndedAt     *time.Time            `json:"ended_at,omitempty"`
	Replay      *ReplayBuffer         `json:"-"`
	Handle      runtime.ProcessHandle `json:"-"`

	// Runtime metrics accumulated from the event stream.
	LastActivity  *time.Time `json:"last_activity,omitempty"`
	InputTokens   int        `json:"input_tokens,omitempty"`
	OutputTokens  int        `json:"output_tokens,omitempty"`
	CostUSD       float64    `json:"cost_usd,omitempty"`
	ToolCallCount int        `json:"tool_call_count,omitempty"`

	mu       sync.Mutex
	resultCh chan struct{} // signals when a result event is received
}

// NewSession creates a session in the Pending state.
// If sessionID is empty, a new UUID is generated.
func NewSession(taskID, agentName, runtimeName string, tags ...map[string]string) *Session {
	return NewSessionWithID("", taskID, agentName, runtimeName, tags...)
}

// NewSessionWithID creates a session with a caller-specified ID.
// If sessionID is empty, a new UUID is generated.
func NewSessionWithID(sessionID, taskID, agentName, runtimeName string, tags ...map[string]string) *Session {
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	var sessionTags map[string]string
	if len(tags) > 0 {
		sessionTags = cloneTags(tags[0])
	}
	return &Session{
		ID:          sessionID,
		TaskID:      taskID,
		AgentName:   agentName,
		RuntimeName: runtimeName,
		Tags:        sessionTags,
		State:       StatePending,
		CreatedAt:   time.Now(),
		Replay:      newLazyReplayBuffer(0),
		resultCh:    make(chan struct{}, 1),
	}
}

// SetRunning transitions the session to Running and attaches the process handle.
func (s *Session) SetRunning(handle runtime.ProcessHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = StateRunning
	s.Handle = handle
}

// SetCompleted transitions the session to Completed or Failed based on exit code.
func (s *Session) SetCompleted(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.ExitCode = &code
	s.EndedAt = &now
	if code == 0 {
		s.State = StateCompleted
	} else {
		s.State = StateFailed
	}
}

// Kill terminates the session's process if one is attached. Thread-safe.
func (s *Session) Kill() error {
	s.mu.Lock()
	h := s.Handle
	s.mu.Unlock()
	if h != nil {
		return h.Kill()
	}
	return nil
}

// RecordActivity updates the LastActivity timestamp to now. Thread-safe.
func (s *Session) RecordActivity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.LastActivity = &now
}

// SetLastActivityForTest sets LastActivity to a specific time. Thread-safe.
// Intended for testing only.
func (s *Session) SetLastActivityForTest(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastActivity = &t
}

// RecordUsage accumulates token counts and cost. Thread-safe.
func (s *Session) RecordUsage(input, output int, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InputTokens += input
	s.OutputTokens += output
	s.CostUSD += cost
}

// SetTag sets a key-value pair on the session's Tags map. Thread-safe.
// Initializes the map if nil.
func (s *Session) SetTag(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Tags == nil {
		s.Tags = make(map[string]string)
	}
	s.Tags[key] = value
}

// RecordToolCall increments the tool call count. Thread-safe.
func (s *Session) RecordToolCall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ToolCallCount++
}

// NotifyResult signals that a result event was received for this session.
// Non-blocking — drops the signal if the channel already has one pending.
func (s *Session) NotifyResult() {
	select {
	case s.resultCh <- struct{}{}:
	default:
	}
}

// ResultCh returns a channel that receives a signal when a result event arrives.
// Used by watchers (e.g. chat manager) to detect turn completion within a live session.
func (s *Session) ResultCh() <-chan struct{} {
	return s.resultCh
}

// Snapshot returns a copy of the session's fields, safe to read without holding the lock.
// Use this before JSON serialization to avoid races with concurrent SetCompleted calls.
func (s *Session) Snapshot() Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastActivity *time.Time
	if s.LastActivity != nil {
		t := *s.LastActivity
		lastActivity = &t
	}
	return Session{
		ID:            s.ID,
		TaskID:        s.TaskID,
		AgentName:     s.AgentName,
		RuntimeName:   s.RuntimeName,
		SessionDir:    s.SessionDir,
		Tags:          cloneTags(s.Tags),
		State:         s.State,
		ExitCode:      s.ExitCode,
		CreatedAt:     s.CreatedAt,
		EndedAt:       s.EndedAt,
		LastActivity:  lastActivity,
		InputTokens:   s.InputTokens,
		OutputTokens:  s.OutputTokens,
		CostUSD:       s.CostUSD,
		ToolCallCount: s.ToolCallCount,
	}
}

func cloneTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(tags))
	for key, value := range tags {
		cloned[key] = value
	}
	return cloned
}

// ErrMaxSessions is returned by Manager.Add when the session limit is reached.
var ErrMaxSessions = fmt.Errorf("max sessions limit reached")

// Manager is a thread-safe registry of active sessions.
type Manager struct {
	mu          sync.RWMutex
	sessions    map[string]*Session
	maxSessions int // 0 = unlimited
}

// NewManager creates an empty session manager.
func NewManager() *Manager {
	return &Manager{sessions: make(map[string]*Session)}
}

// SetMaxSessions configures the maximum number of concurrent sessions.
// 0 means unlimited (the default).
func (m *Manager) SetMaxSessions(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxSessions = n
}

// Add registers a session. Returns error if the ID already exists or the
// max sessions limit has been reached.
func (m *Manager) Add(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		return ErrMaxSessions
	}
	if _, exists := m.sessions[s.ID]; exists {
		return fmt.Errorf("session %s already exists", s.ID)
	}
	m.sessions[s.ID] = s
	return nil
}

// Get returns the session with the given ID, or nil if not found.
func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// Remove deletes a session from the registry.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// List returns all sessions.
func (m *Manager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// ShutdownAll kills all active sessions, closes their replay buffers, and
// removes them from the registry. Used during graceful daemon shutdown.
func (m *Manager) ShutdownAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()

	for _, s := range all {
		_ = s.Kill()
		s.Replay.Close()
		s.SetCompleted(-1)
	}

	m.mu.Lock()
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()
}

// Recover re-registers sessions recovered by a runtime (e.g., Docker containers
// that survived a daemon restart). Sets their state to Orphaned.
func (m *Manager) Recover(handles []runtime.ProcessHandle, runtimeName string) []*Session {
	var recovered []*Session
	for _, h := range handles {
		sessionID := uuid.New().String()
		taskID := ""
		if info := h.RecoveryInfo(); info != nil {
			if info.SessionID != "" {
				sessionID = info.SessionID
			}
			taskID = info.TaskID
		}
		s := &Session{
			ID:          sessionID,
			TaskID:      taskID,
			RuntimeName: runtimeName,
			State:       StateOrphaned,
			CreatedAt:   time.Now(),
			Replay:      newLazyReplayBuffer(0),
			Handle:      h,
		}
		m.mu.Lock()
		m.sessions[s.ID] = s
		m.mu.Unlock()
		recovered = append(recovered, s)
	}
	return recovered
}
