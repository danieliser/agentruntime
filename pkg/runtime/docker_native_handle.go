package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// nativeDockerHandle exposes a detached container's native provider stdio.
// Input is delivered through docker attach while output is read from Docker's
// retained log stream so records emitted before attachment remain replayable.
type nativeDockerHandle struct {
	containerID string
	dockerHost  string
	recovery    RecoveryInfo

	attachCmd *exec.Cmd
	logsCmd   *exec.Cmd
	waitCmd   *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	done      chan ExitResult
	killMu    sync.Mutex
}

func newNativeDockerHandle(host, containerID string, recovery RecoveryInfo) (*nativeDockerHandle, error) {
	attachCmd := nativeDockerCommand(host, "attach", "--sig-proxy=false", containerID)
	stdin, err := attachCmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("attach stdin pipe: %w", err)
	}
	attachCmd.Stdout = io.Discard
	attachCmd.Stderr = io.Discard
	if err := attachCmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start attach: %w", err)
	}

	logsCmd := nativeDockerCommand(host, "logs", "--follow", "--since=0", containerID)
	stdout, err := logsCmd.StdoutPipe()
	if err != nil {
		stopDockerCLI(attachCmd)
		return nil, fmt.Errorf("logs stdout pipe: %w", err)
	}
	stderr, err := logsCmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		stopDockerCLI(attachCmd)
		return nil, fmt.Errorf("logs stderr pipe: %w", err)
	}
	if err := logsCmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		stopDockerCLI(attachCmd)
		return nil, fmt.Errorf("start logs: %w", err)
	}

	var waitStdout bytes.Buffer
	var waitStderr bytes.Buffer
	waitCmd := nativeDockerCommand(host, "wait", containerID)
	waitCmd.Stdout = &waitStdout
	waitCmd.Stderr = &waitStderr
	if err := waitCmd.Start(); err != nil {
		stopDockerCLI(logsCmd)
		stopDockerCLI(attachCmd)
		return nil, fmt.Errorf("start wait: %w", err)
	}

	handle := &nativeDockerHandle{
		containerID: containerID,
		dockerHost:  host,
		recovery:    recovery,
		attachCmd:   attachCmd,
		logsCmd:     logsCmd,
		waitCmd:     waitCmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		done:        make(chan ExitResult, 1),
	}
	go func() {
		err := waitCmd.Wait()
		if err != nil {
			handle.done <- ExitResult{Code: -1, Err: dockerCommandError(err, waitStderr.String())}
			return
		}
		code, err := strconv.Atoi(strings.TrimSpace(waitStdout.String()))
		if err != nil {
			handle.done <- ExitResult{Code: -1, Err: fmt.Errorf("parse docker wait exit code %q: %w", strings.TrimSpace(waitStdout.String()), err)}
			return
		}
		handle.done <- ExitResult{Code: code}
	}()
	go func() { _ = attachCmd.Wait() }()
	go func() { _ = logsCmd.Wait() }()
	return handle, nil
}

func nativeDockerCommand(host string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(context.Background(), "docker", args...)
	if host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
	}
	return cmd
}

func stopDockerCLI(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func (h *nativeDockerHandle) Stdin() io.WriteCloser   { return h.stdin }
func (h *nativeDockerHandle) Stdout() io.ReadCloser   { return h.stdout }
func (h *nativeDockerHandle) Stderr() io.ReadCloser   { return h.stderr }
func (h *nativeDockerHandle) Wait() <-chan ExitResult { return h.done }
func (h *nativeDockerHandle) PID() int                { return 0 }
func (h *nativeDockerHandle) RuntimeID() string       { return h.containerID }
func (*nativeDockerHandle) NativeStdio() bool         { return true }

func (h *nativeDockerHandle) RecoveryInfo() *RecoveryInfo {
	if h.recovery.SessionID == "" && h.recovery.TaskID == "" {
		return nil
	}
	copy := h.recovery
	return &copy
}

func (h *nativeDockerHandle) Kill() error {
	h.killMu.Lock()
	defer h.killMu.Unlock()
	_, err := dockerOutputHost(context.Background(), h.dockerHost, "kill", h.containerID)
	return err
}
