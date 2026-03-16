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
	Tags        map[string]string     `json:"tags,omitempty"`
	State       State                 `json:"state"`
	ExitCode    *int                  `json:"exit_code,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	EndedAt     *time.Time            `json:"ended_at,omitempty"`
	Replay      *ReplayBuffer         `json:"-"`
	Handle      runtime.ProcessHandle `json:"-"`

	mu sync.Mutex
}

// NewSession creates a session in the Pending state.
func NewSession(taskID, agentName, runtimeName string, tags ...map[string]string) *Session {
	var sessionTags map[string]string
	if len(tags) > 0 {
		sessionTags = cloneTags(tags[0])
	}
	return &Session{
		ID:          uuid.New().String(),
		TaskID:      taskID,
		AgentName:   agentName,
		RuntimeName: runtimeName,
		Tags:        sessionTags,
		State:       StatePending,
		CreatedAt:   time.Now(),
		Replay:      newLazyReplayBuffer(0),
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

// Snapshot returns a copy of the session's fields, safe to read without holding the lock.
// Use this before JSON serialization to avoid races with concurrent SetCompleted calls.
func (s *Session) Snapshot() Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Session{
		ID:          s.ID,
		TaskID:      s.TaskID,
		AgentName:   s.AgentName,
		RuntimeName: s.RuntimeName,
		Tags:        cloneTags(s.Tags),
		State:       s.State,
		ExitCode:    s.ExitCode,
		CreatedAt:   s.CreatedAt,
		EndedAt:     s.EndedAt,
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

// Manager is a thread-safe registry of active sessions.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewManager creates an empty session manager.
func NewManager() *Manager {
	return &Manager{sessions: make(map[string]*Session)}
}

// Add registers a session. Returns error if the ID already exists.
func (m *Manager) Add(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// Recover re-registers sessions recovered by a runtime (e.g., Docker containers
// that survived a daemon restart). Sets their state to Orphaned.
func (m *Manager) Recover(handles []runtime.ProcessHandle, runtimeName string) []*Session {
	var recovered []*Session
	for _, h := range handles {
		s := &Session{
			ID:          uuid.New().String(),
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
