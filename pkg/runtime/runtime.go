// Package runtime defines the Runtime interface for spawning and managing
// agent processes across different execution environments.
package runtime

import (
	"context"
	"fmt"
	"io"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

// Runtime is the core abstraction for agent process execution. Each runtime
// implementation knows how to spawn processes in its environment (local OS,
// Docker container, SSH host) and recover orphaned sessions from a previous
// daemon run.
type Runtime interface {
	// Spawn creates a new agent process with the given configuration.
	// Returns a ProcessHandle for interacting with the process stdio.
	Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error)

	// Recover finds any orphaned sessions from a previous daemon run
	// and returns handles to them. This enables session continuity
	// across daemon restarts for runtimes that support it.
	Recover(ctx context.Context) ([]ProcessHandle, error)

	// Name returns the runtime identifier ("local", "docker", "ssh").
	Name() string

	// Cleanup performs graceful teardown of runtime-managed infrastructure
	// (e.g., proxy containers, networks). Safe to call even if nothing was started.
	Cleanup(ctx context.Context) error
}

// SpawnConfig holds the parameters for spawning an agent process.
type SpawnConfig struct {
	// SessionID identifies the owning session and is used for container naming/labels.
	SessionID string

	// Generation and idempotency fields label reconstructable durable work.
	// They are empty for legacy, non-durable callers.
	Generation     int64
	IdempotencyKey string
	RequestHash    string

	// AgentName identifies the agent type ("claude", "codex").
	AgentName string

	// Cmd is the command and arguments to execute.
	Cmd []string

	// Prompt is the initial user prompt for runtimes that deliver turns over a
	// control channel instead of embedding them in Cmd.
	Prompt string

	// Model overrides the agent's default model (e.g. "claude-opus-4-5").
	// Threaded into AGENT_CONFIG for the sidecar.
	Model string

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

	// VolumeName is the Docker volume name to mount (for persistent sessions).
	// If empty, a new volume is created based on SessionID.
	VolumeName string

	// PTY requests a pseudo-terminal allocation. Not all runtimes support this.
	PTY bool
}

// RuntimeIdentifiedHandle exposes the stable runtime process/container ID
// needed to persist a reconstructable generation.
type RuntimeIdentifiedHandle interface {
	RuntimeID() string
}

// RuntimeImageIdentifiedHandle exposes the immutable image/binary digest that
// produced a concrete runtime generation.
type RuntimeImageIdentifiedHandle interface {
	RuntimeImageDigest() string
}

// NativeStdioHandle marks a process handle whose stdio is the provider's exact
// native protocol rather than a compatibility bridge.
type NativeStdioHandle interface {
	NativeStdio() bool
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

// SteerableHandle is retained only for the unversioned compatibility bridge.
// Native v1 controls use nativeprotocol.Transport instead.
type SteerableHandle interface {
	ProcessHandle

	// SendPrompt sends a prompt through a compatibility handle.
	SendPrompt(content string) error

	// SendInterrupt sends an interrupt through a compatibility handle.
	SendInterrupt() error

	// SendSteer sends steering input through a compatibility handle.
	SendSteer(content string) error

	// SendContext sends a context command with text and/or file path.
	SendContext(text, filePath string) error

	// SendMention sends a mention command referencing a file location.
	SendMention(filePath string, lineStart, lineEnd int) error
}

// Compile-time interface assertions.
var (
	_ Runtime       = (*LocalRuntime)(nil)
	_ Runtime       = (*DockerRuntime)(nil)
	_ ProcessHandle = (*localHandle)(nil)
	_ ProcessHandle = (*dockerHandle)(nil)
	_ ProcessHandle = (*nativeDockerHandle)(nil)
	_ ProcessHandle = (*recoveredDockerHandle)(nil)
)

// ErrNotSteerable is returned by the unversioned bridge when its compatibility
// handle cannot translate a structured control.
var ErrNotSteerable = fmt.Errorf("handle does not support structured compatibility controls")

// ExitResult holds the outcome of a process termination.
type ExitResult struct {
	// Code is the process exit code. 0 indicates success.
	Code int

	// Err is any error encountered waiting for the process, distinct from a non-zero exit code.
	Err error

	// ErrorDetail contains runtime-specific terminal detail, if present.
	ErrorDetail string
}

// RecoveryInfo carries stable identifiers for a recovered process handle.
type RecoveryInfo struct {
	SessionID      string
	TaskID         string
	AgentName      string
	Generation     int64
	IdempotencyKey string
	RequestHash    string
}
