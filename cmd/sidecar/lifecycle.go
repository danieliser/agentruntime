package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	sidecarGracePeriod = 5 * time.Second
)

// HookRunner executes lifecycle hook scripts and emits their output as
// system events through the sidecar's event stream.
type HookRunner struct {
	config    *LifecycleConfig
	sessionID string
	taskID    string
	agentName string
	workDir   string
	broadcast func(Event) error
}

// NewHookRunner creates a HookRunner from lifecycle config. Returns nil if
// no hooks are configured, so callers can nil-check to skip hook logic.
func NewHookRunner(cfg *LifecycleConfig, sessionID, taskID, agentName, workDir string, broadcast func(Event) error) *HookRunner {
	if cfg == nil || !cfg.HasHooks() {
		return nil
	}
	return &HookRunner{
		config:    cfg,
		sessionID: sessionID,
		taskID:    taskID,
		agentName: agentName,
		workDir:   workDir,
		broadcast: broadcast,
	}
}

// baseEnv returns the environment variables common to all hooks.
func (h *HookRunner) baseEnv() []string {
	return []string{
		"SESSION_ID=" + h.sessionID,
		"TASK_ID=" + h.taskID,
		"AGENT=" + h.agentName,
		"WORK_DIR=" + h.workDir,
	}
}

// RunBlocking executes a hook script synchronously with a timeout.
// The script's stdout/stderr is captured line-by-line and emitted as
// system events with source "hook:<name>".
//
// Returns nil if the script path is empty (hook not configured).
// Returns an error if the script exits non-zero or the timeout is exceeded.
func (h *HookRunner) RunBlocking(ctx context.Context, name, path string, timeoutSec int, extraEnv map[string]string) error {
	if path == "" {
		return nil
	}

	// Check that the script exists before trying to execute.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("[lifecycle] %s: script not found: %s (skipping)", name, path)
		return nil
	}

	timeout := time.Duration(timeoutSec) * time.Second
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, path)
	cmd.Dir = h.workDir
	cmd.Env = h.buildEnv(extraEnv)

	// Capture stdout and stderr through pipes for line-by-line streaming.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: stdout pipe: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s: stderr pipe: %w", name, err)
	}

	h.emitSystem(name, fmt.Sprintf("running %s hook: %s", name, path))

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: start: %w", name, err)
	}

	// Stream output lines as system events.
	// Use a WaitGroup to ensure all output is captured before returning.
	source := "hook:" + name
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			h.emitHookOutput(source, scanner.Text())
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			h.emitHookOutput(source, scanner.Text())
		}
	}()

	// Wait for the process first — this closes the pipes, which unblocks scanners.
	waitErr := cmd.Wait()
	wg.Wait()

	if hookCtx.Err() == context.DeadlineExceeded {
		h.emitSystem(name, fmt.Sprintf("%s hook timed out after %s", name, timeout))
		return fmt.Errorf("%s: timed out after %s", name, timeout)
	}

	if waitErr != nil {
		h.emitSystem(name, fmt.Sprintf("%s hook failed: %v", name, waitErr))
		return fmt.Errorf("%s: %w", name, waitErr)
	}

	h.emitSystem(name, fmt.Sprintf("%s hook completed successfully", name))
	return nil
}

// SpawnBackground starts a hook script as a background process. The script's
// stdout/stderr is streamed as system events. Returns a cancel function that
// sends SIGTERM, waits for the grace period, then SIGKILL.
//
// Returns a nil cancel func if the script path is empty.
func (h *HookRunner) SpawnBackground(ctx context.Context, name, path string, extraEnv map[string]string) (cancel func(), err error) {
	if path == "" {
		return nil, nil
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("[lifecycle] %s: script not found: %s (skipping)", name, path)
		return nil, nil
	}

	cmd := exec.CommandContext(ctx, path)
	cmd.Dir = h.workDir
	cmd.Env = h.buildEnv(extraEnv)
	// Set process group so we can signal the entire group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stdout pipe: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stderr pipe: %w", name, err)
	}

	h.emitSystem(name, fmt.Sprintf("spawning %s hook: %s", name, path))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: start: %w", name, err)
	}

	source := "hook:" + name

	// Stream output in background goroutines.
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			h.emitHookOutput(source, scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			h.emitHookOutput(source, scanner.Text())
		}
	}()

	// Wait for the process in the background to avoid zombies.
	doneCh := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(doneCh)
	}()

	cancelFn := func() {
		h.emitSystem(name, "stopping sidecar hook")

		// Send SIGTERM to process group.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}

		// Wait for grace period, then SIGKILL.
		select {
		case <-doneCh:
			// Exited cleanly.
		case <-time.After(sidecarGracePeriod):
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			<-doneCh
		}
		h.emitSystem(name, "sidecar hook stopped")
	}

	return cancelFn, nil
}

// buildEnv constructs the full environment for a hook process.
func (h *HookRunner) buildEnv(extraEnv map[string]string) []string {
	env := append(os.Environ(), h.baseEnv()...)
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	return env
}

// emitSystem emits a system event for hook lifecycle messages.
func (h *HookRunner) emitSystem(hookName, text string) {
	if h.broadcast == nil {
		return
	}
	_ = h.broadcast(Event{
		Type: "system",
		Data: map[string]any{
			"source":  "hook:" + hookName,
			"subtype": "lifecycle",
			"text":    text,
		},
	})
}

// emitHookOutput emits a system event for a line of hook stdout/stderr.
func (h *HookRunner) emitHookOutput(source, text string) {
	if h.broadcast == nil || text == "" {
		return
	}
	_ = h.broadcast(Event{
		Type: "system",
		Data: map[string]any{
			"source": source,
			"text":   text,
		},
	})
}
