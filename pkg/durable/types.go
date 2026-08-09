// Package durable defines AgentD's DUR-101 persistence contract for logical
// sessions, runtime generations, immutable events, and terminal receipts.
package durable

import (
	"encoding/json"
	"time"
)

// SessionState is the durable lifecycle state of one logical session.
type SessionState string

const (
	StateCreated       SessionState = "created"
	StateStarting      SessionState = "starting"
	StateRunning       SessionState = "running"
	StateCompleted     SessionState = "completed"
	StateFailed        SessionState = "failed"
	StateCancelled     SessionState = "cancelled"
	StateTimedOut      SessionState = "timed_out"
	StateCrashed       SessionState = "crashed"
	StateIndeterminate SessionState = "indeterminate"
)

// Terminal reports whether no further work may run under this logical session.
func (state SessionState) Terminal() bool {
	switch state {
	case StateCompleted, StateFailed, StateCancelled, StateTimedOut, StateCrashed, StateIndeterminate:
		return true
	default:
		return false
	}
}

// GenerationState is the lifecycle state of one concrete runtime generation.
type GenerationState string

const (
	GenerationStarting      GenerationState = "starting"
	GenerationRunning       GenerationState = "running"
	GenerationExited        GenerationState = "exited"
	GenerationLost          GenerationState = "lost"
	GenerationIndeterminate GenerationState = "indeterminate"
)

// Terminal reports whether the generation can no longer accept work.
func (state GenerationState) Terminal() bool {
	switch state {
	case GenerationExited, GenerationLost, GenerationIndeterminate:
		return true
	default:
		return false
	}
}

// Stream identifies the source of an immutable event record.
type Stream string

const (
	StreamProviderStdout Stream = "provider_stdout"
	StreamRuntimeStderr  Stream = "runtime_stderr"
	StreamControl        Stream = "control"
	StreamLifecycle      Stream = "lifecycle"
	StreamTerminal       Stream = "terminal"
)

// Session is the durable identity and state of one logical execution.
type Session struct {
	ID               string
	IdempotencyKey   string
	RequestHash      string
	RequestManifest  json.RawMessage
	SecretGrants     []string
	Agent            string
	Runtime          string
	State            SessionState
	ActiveGeneration int64
	LastSequence     int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CreateSessionParams contains immutable inputs for idempotent session creation.
type CreateSessionParams struct {
	SessionID       string
	IdempotencyKey  string
	RequestHash     string
	RequestManifest json.RawMessage
	SecretGrants    []string
	Agent           string
	Runtime         string
	CreatedAt       time.Time
}

// CreateSessionResult distinguishes first creation from idempotent lookup.
type CreateSessionResult struct {
	Session Session
	Created bool
}

// TransitionSessionParams is a compare-and-set lifecycle transition.
type TransitionSessionParams struct {
	SessionID string
	From      SessionState
	To        SessionState
	At        time.Time
}

// Generation identifies one concrete runtime process/container incarnation.
type Generation struct {
	SessionID        string
	Number           int64
	Runtime          string
	State            GenerationState
	ContainerID      string
	ImageReference   string
	ImageDigest      string
	SandboxProfile   string
	ProviderID       string
	DockerLogDriver  string
	DockerLogOptions json.RawMessage
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CreateGenerationParams describes a newly admitted runtime generation.
type CreateGenerationParams struct {
	SessionID        string
	Runtime          string
	ContainerID      string
	ImageReference   string
	ImageDigest      string
	SandboxProfile   string
	ProviderID       string
	DockerLogDriver  string
	DockerLogOptions json.RawMessage
	CreatedAt        time.Time
}

// TransitionGenerationParams is a compare-and-set generation transition.
type TransitionGenerationParams struct {
	SessionID  string
	Generation int64
	From       GenerationState
	To         GenerationState
	At         time.Time
}

// Event is the stable AgentD envelope around one exact native record.
type Event struct {
	SchemaVersion string
	EventID       string
	SessionID     string
	Generation    int64
	Sequence      int64
	Timestamp     time.Time
	Type          string
	Stream        Stream
	Payload       json.RawMessage
	Raw           []byte
	RawSHA256     string
}

// AppendEventParams omits Sequence, which the store allocates atomically.
type AppendEventParams struct {
	SchemaVersion string
	EventID       string
	SessionID     string
	Generation    int64
	Timestamp     time.Time
	Type          string
	Stream        Stream
	Payload       json.RawMessage
	Raw           []byte
}

// AppendEventResult distinguishes first append from an idempotent retry.
type AppendEventResult struct {
	Event   Event
	Created bool
}

// EventQuery requests events strictly after a durable sequence cursor.
type EventQuery struct {
	SessionID     string
	AfterSequence int64
	Limit         int
}

// EventPage returns committed events and the session's current durable tail.
type EventPage struct {
	Events       []Event
	LastSequence int64
	HasMore      bool
}

// TerminalReceipt is the immutable final proof for a logical session.
type TerminalReceipt struct {
	SessionID    string
	Generation   int64
	State        SessionState
	ExitCode     *int
	Signal       string
	StartedAt    time.Time
	EndedAt      time.Time
	OutputHash   string
	ArtifactHash string
	LastSequence int64
}

// FinalizeSessionParams atomically closes the runtime generation, transitions
// the logical session, and commits its immutable receipt.
type FinalizeSessionParams struct {
	From           SessionState
	GenerationFrom GenerationState
	GenerationTo   GenerationState
	Receipt        TerminalReceipt
}

// FinalizeSessionResult distinguishes first finalization from an idempotent retry.
type FinalizeSessionResult struct {
	Session Session
	Receipt TerminalReceipt
	Created bool
}
