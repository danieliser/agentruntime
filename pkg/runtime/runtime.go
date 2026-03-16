// Package runtime defines the Runtime interface for spawning and managing
// agent processes across different execution environments.
package runtime

import (
	"context"
	"io"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

// Runtime is the core abstraction for agent process execution. Each runtime
// implementation knows how to spawn processes in its environment (local OS,
// Docker container, OpenSandbox, SSH host) and recover orphaned sessions
// from a previous daemon run.
type Runtime interface {
	// Spawn creates a new agent process with the given configuration.
	// Returns a ProcessHandle for interacting with the process stdio.
	Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error)

	// Recover finds any orphaned sessions from a previous daemon run
	// and returns handles to them. This enables session continuity
	// across daemon restarts for runtimes that support it.
	Recover(ctx context.Context) ([]ProcessHandle, error)

	// Name returns the runtime identifier ("local", "docker", "opensandbox", "ssh").
	Name() string
}

// SpawnConfig holds the parameters for spawning an agent process.
type SpawnConfig struct {
	// SessionID identifies the owning session and is used for container naming/labels.
	SessionID string

	// AgentName identifies the agent type ("claude", "codex", "opencode").
	AgentName string

	// Cmd is the command and arguments to execute.
	Cmd []string

	// Env is additional environment variables for the process.
	Env map[string]string

	// WorkDir is the working directory for the process.
	WorkDir string

	// TaskID is the unique identifier for this task, used for session naming.
	TaskID string

	// Request carries the full session request for runtimes that need mounts,
	// container resources, or agent-config materialization.
	Request *apischema.SessionRequest

	// SessionDir points to a location where runtimes can publish the host path
	// to any materialized per-session files they create.
	SessionDir *string

	// PTY requests a pseudo-terminal allocation. Not all runtimes support this.
	PTY bool
}

// ProcessHandle provides access to a running agent process's stdio streams
// and lifecycle. It is the runtime-agnostic interface that the bridge and
// session manager interact with.
type ProcessHandle interface {
	// Stdin returns a writer connected to the process's standard input.
	Stdin() io.WriteCloser

	// Stdout returns a reader connected to the process's standard output.
	Stdout() io.ReadCloser

	// Stderr returns a reader connected to the process's standard error.
	// Returns nil if the process is using a PTY (stderr merged into stdout).
	Stderr() io.ReadCloser

	// Wait returns a channel that receives the exit result when the process terminates.
	Wait() <-chan ExitResult

	// Kill terminates the process immediately.
	Kill() error

	// PID returns the OS process ID. Returns 0 if not applicable (e.g., remote runtime).
	PID() int

	// RecoveryInfo returns metadata captured during orphan recovery.
	// Non-recovered handles should return nil.
	RecoveryInfo() *RecoveryInfo
}

// ExitResult holds the outcome of a process termination.
type ExitResult struct {
	// Code is the process exit code. 0 indicates success.
	Code int

	// Err is any error encountered waiting for the process, distinct from a non-zero exit code.
	Err error
}

// RecoveryInfo carries stable identifiers for a recovered process handle.
type RecoveryInfo struct {
	SessionID string
	TaskID    string
}
