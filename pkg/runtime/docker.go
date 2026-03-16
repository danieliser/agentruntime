package runtime

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// DockerConfig holds configuration for the Docker runtime.
type DockerConfig struct {
	// Image is the default container image (e.g., "alpine:latest").
	Image string

	// Network is the Docker network to attach containers to.
	Network string

	// ExtraArgs are additional arguments passed to docker run.
	ExtraArgs []string
}

// DockerRuntime spawns agent processes inside Docker containers using the
// docker CLI. Containers are labeled with agentruntime.task_id for recovery.
type DockerRuntime struct {
	cfg DockerConfig
}

// NewDockerRuntime creates a new Docker runtime with the given configuration.
func NewDockerRuntime(cfg DockerConfig) *DockerRuntime {
	return &DockerRuntime{cfg: cfg}
}

// labelKey is the Docker label used to identify agentruntime containers.
const labelKey = "agentruntime.task_id"

func (r *DockerRuntime) Name() string { return "docker" }

// Spawn runs a command inside a Docker container. The container is created with
// stdin attached and labeled with the task ID for recovery.
func (r *DockerRuntime) Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error) {
	if len(cfg.Cmd) == 0 {
		return nil, &SpawnError{Reason: "cmd is empty"}
	}
	if r.cfg.Image == "" {
		return nil, &SpawnError{Reason: "no container image configured"}
	}

	// Build docker run command.
	args := []string{"run", "--rm", "-i"}

	// Label for recovery.
	taskID := cfg.TaskID
	if taskID == "" {
		taskID = "unknown"
	}
	args = append(args, "--label", fmt.Sprintf("%s=%s", labelKey, taskID))

	// Working directory.
	if cfg.WorkDir != "" {
		args = append(args, "-w", cfg.WorkDir)
	}

	// Environment variables.
	for k, v := range cfg.Env {
		args = append(args, "-e", k+"="+v)
	}

	// Network.
	if r.cfg.Network != "" {
		args = append(args, "--network", r.cfg.Network)
	}

	// Extra args.
	args = append(args, r.cfg.ExtraArgs...)

	// Image + command.
	args = append(args, r.cfg.Image)
	args = append(args, cfg.Cmd...)

	cmd := exec.CommandContext(ctx, "docker", args...)

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
		return nil, &SpawnError{Reason: "docker run start", Err: err}
	}

	done := make(chan ExitResult, 1)
	go func() {
		waitErr := cmd.Wait()
		code := 0
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
				waitErr = nil
			}
		}
		done <- ExitResult{Code: code, Err: waitErr}
	}()

	return &dockerHandle{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		done:   done,
	}, nil
}

// Recover finds running containers with the agentruntime label and returns
// handles to them. This enables session recovery after daemon restarts.
func (r *DockerRuntime) Recover(ctx context.Context) ([]ProcessHandle, error) {
	// List running containers with our label.
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q",
		"--filter", fmt.Sprintf("label=%s", labelKey),
	).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var handles []ProcessHandle
	for _, id := range lines {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		// Attach to the running container's logs for stdout.
		// This is a best-effort recovery — full stdio reattach would need docker attach.
		handles = append(handles, &recoveredDockerHandle{containerID: id})
	}
	return handles, nil
}

// dockerHandle wraps a docker run subprocess.
type dockerHandle struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	done   chan ExitResult
}

func (h *dockerHandle) Stdin() io.WriteCloser  { return h.stdin }
func (h *dockerHandle) Stdout() io.ReadCloser  { return h.stdout }
func (h *dockerHandle) Stderr() io.ReadCloser  { return h.stderr }
func (h *dockerHandle) Wait() <-chan ExitResult { return h.done }

func (h *dockerHandle) Kill() error {
	if h.cmd.Process != nil {
		return h.cmd.Process.Kill()
	}
	return nil
}

func (h *dockerHandle) PID() int {
	if h.cmd.Process != nil {
		return h.cmd.Process.Pid
	}
	return 0
}

// recoveredDockerHandle is a minimal handle for containers found during recovery.
// It can kill the container but doesn't have full stdio access.
type recoveredDockerHandle struct {
	containerID string
}

func (h *recoveredDockerHandle) Stdin() io.WriteCloser  { return nil }
func (h *recoveredDockerHandle) Stdout() io.ReadCloser  { return nil }
func (h *recoveredDockerHandle) Stderr() io.ReadCloser  { return nil }
func (h *recoveredDockerHandle) PID() int               { return 0 }

func (h *recoveredDockerHandle) Wait() <-chan ExitResult {
	// Recovered containers need explicit management.
	ch := make(chan ExitResult)
	return ch
}

func (h *recoveredDockerHandle) Kill() error {
	return exec.Command("docker", "kill", h.containerID).Run()
}
