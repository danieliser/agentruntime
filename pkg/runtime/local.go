package runtime

import (
	"context"
	"io"
	"os/exec"
	"sync"
)

// LocalRuntime spawns agent processes as local OS subprocesses.
type LocalRuntime struct{}

// NewLocalRuntime creates a new local runtime.
func NewLocalRuntime() *LocalRuntime {
	return &LocalRuntime{}
}

func (r *LocalRuntime) Name() string { return "local" }

// Spawn starts a local subprocess with the given configuration.
func (r *LocalRuntime) Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error) {
	if len(cfg.Cmd) == 0 {
		return nil, &SpawnError{Reason: "cmd is empty"}
	}

	cmd := exec.CommandContext(ctx, cfg.Cmd[0], cfg.Cmd[1:]...)
	cmd.Dir = cfg.WorkDir
	configureLocalProcessGroup(cmd)

	env, err := buildSpawnEnv(cfg.Env)
	if err != nil {
		return nil, &SpawnError{Reason: "env", Err: err}
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &SpawnError{Reason: "stdin pipe", Err: err}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &SpawnError{Reason: "stdout pipe", Err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, &SpawnError{Reason: "stderr pipe", Err: err}
	}

	if err := cmd.Start(); err != nil {
		return nil, &SpawnError{Reason: "start", Err: err}
	}

	raw := make(chan ExitResult, 1)
	h := &localHandle{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		raw:    raw,
	}

	go func() {
		waitErr := cmd.Wait()
		code := 0
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
				waitErr = nil // non-zero exit is not an error
			}
		}
		raw <- ExitResult{Code: code, Err: waitErr}
	}()
	go h.fanoutLoop()

	return h, nil
}

// Recover returns an empty slice — local processes don't survive daemon restarts.
func (r *LocalRuntime) Recover(_ context.Context) ([]ProcessHandle, error) {
	return nil, nil
}

// localHandle wraps an os/exec.Cmd into the ProcessHandle interface.
// Wait() fans out the single process-exit event to multiple callers.
type localHandle struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	// raw is written exactly once by the cmd.Wait goroutine.
	raw chan ExitResult

	// fanout guards subscriber list and cached result.
	fanMu  sync.Mutex
	fanSub []chan ExitResult
	fanRes *ExitResult
}

// fanoutLoop drains raw once, caches the result, and broadcasts to all subscribers.
func (h *localHandle) fanoutLoop() {
	result := <-h.raw
	h.fanMu.Lock()
	h.fanRes = &result
	subs := h.fanSub
	h.fanSub = nil
	h.fanMu.Unlock()
	for _, ch := range subs {
		ch <- result
	}
}

func (h *localHandle) Stdin() io.WriteCloser { return h.stdin }
func (h *localHandle) Stdout() io.ReadCloser { return h.stdout }
func (h *localHandle) Stderr() io.ReadCloser { return h.stderr }

// Wait returns a channel that receives the process exit result exactly once.
// Safe to call multiple times — each caller gets its own channel.
func (h *localHandle) Wait() <-chan ExitResult {
	ch := make(chan ExitResult, 1)
	h.fanMu.Lock()
	if h.fanRes != nil {
		// Already exited — deliver immediately.
		ch <- *h.fanRes
		h.fanMu.Unlock()
		return ch
	}
	h.fanSub = append(h.fanSub, ch)
	h.fanMu.Unlock()
	return ch
}

func (h *localHandle) Kill() error {
	if h.cmd.Process != nil {
		return killLocalProcessGroup(h.cmd)
	}
	return nil
}

func (h *localHandle) PID() int {
	if h.cmd.Process != nil {
		return h.cmd.Process.Pid
	}
	return 0
}

func (h *localHandle) RecoveryInfo() *RecoveryInfo { return nil }

// SpawnError wraps errors from the spawn process.
type SpawnError struct {
	Reason string
	Err    error
}

func (e *SpawnError) Error() string {
	if e.Err != nil {
		return "spawn: " + e.Reason + ": " + e.Err.Error()
	}
	return "spawn: " + e.Reason
}

func (e *SpawnError) Unwrap() error { return e.Err }
