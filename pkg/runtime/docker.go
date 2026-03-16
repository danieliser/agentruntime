package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/materialize"
)

// DockerConfig holds configuration for the Docker runtime.
type DockerConfig struct {
	// Image is the default container image (e.g., "alpine:latest").
	Image string

	// Network is the Docker network to attach containers to.
	Network string

	// DataDir is the persistent agentruntime data directory for session homes.
	DataDir string

	// ExtraArgs are additional arguments passed to docker run.
	ExtraArgs []string
}

// DockerRuntime spawns agent processes inside Docker containers using the
// docker CLI. Containers are labeled with task/session identifiers for recovery.
type DockerRuntime struct {
	cfg          DockerConfig
	materializer dockerMaterializer
}

// NewDockerRuntime creates a new Docker runtime with the given configuration.
func NewDockerRuntime(cfg DockerConfig) *DockerRuntime {
	return &DockerRuntime{
		cfg: cfg,
		materializer: dockerMaterializerFunc(func(req *apischema.SessionRequest, sessionID string) (*materialize.Result, error) {
			return materialize.Materialize(req, sessionID, cfg.DataDir)
		}),
	}
}

const (
	dockerTaskLabelKey    = "agentruntime.task_id"
	dockerSessionLabelKey = "agentruntime.session_id"
)

type dockerMaterializer interface {
	Materialize(req *apischema.SessionRequest, sessionID string) (*materialize.Result, error)
}

type dockerMaterializerFunc func(req *apischema.SessionRequest, sessionID string) (*materialize.Result, error)

func (f dockerMaterializerFunc) Materialize(req *apischema.SessionRequest, sessionID string) (*materialize.Result, error) {
	return f(req, sessionID)
}

type dockerRunSpec struct {
	args    []string
	cleanup func()
}

func (r *DockerRuntime) Name() string { return "docker" }

// Spawn runs a command inside a Docker container. The container is created with
// stdin attached and labeled for orphan recovery.
func (r *DockerRuntime) Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error) {
	if len(cfg.Cmd) == 0 {
		return nil, &SpawnError{Reason: "cmd is empty"}
	}
	spec, err := r.prepareRun(cfg)
	if err != nil {
		return nil, &SpawnError{Reason: "docker run args", Err: err}
	}

	cmd := exec.CommandContext(ctx, "docker", spec.args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "stdin pipe", Err: err}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "stdout pipe", Err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "stderr pipe", Err: err}
	}

	if err := cmd.Start(); err != nil {
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "docker run start", Err: err}
	}
	// NOTE: spec.cleanup (which removes temp artifacts after materialization)
	// must NOT run until after the container has started and read its files.
	// We defer it to after cmd.Wait() completes in the goroutine below.

	done := make(chan ExitResult, 1)
	go func() {
		waitErr := cmd.Wait()
		// Now safe to clean up temp files — container has exited.
		if spec.cleanup != nil {
			spec.cleanup()
		}
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

func (r *DockerRuntime) buildRunArgs(cfg SpawnConfig) ([]string, error) {
	spec, err := r.prepareRun(cfg)
	if err != nil {
		return nil, err
	}
	if spec.cleanup != nil {
		defer spec.cleanup()
	}
	return spec.args, nil
}

func (r *DockerRuntime) prepareRun(cfg SpawnConfig) (*dockerRunSpec, error) {
	req := cfg.Request
	image := r.cfg.Image
	if req != nil && req.Container != nil && req.Container.Image != "" {
		image = req.Container.Image
	}
	if image == "" {
		return nil, fmt.Errorf("no container image configured")
	}

	cleanups := make([]func(), 0, 2)
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			if cleanups[i] != nil {
				cleanups[i]()
			}
		}
	}

	mounts := requestMounts(cfg)
	if req != nil && (req.Claude != nil || req.Codex != nil) {
		result, err := r.materializer.Materialize(req, cfg.SessionID)
		if err != nil {
			return nil, err
		}
		if cfg.SessionDir != nil {
			*cfg.SessionDir = result.SessionDir
		}
		cleanups = append(cleanups, result.CleanupFn)
		mounts = append(mounts, result.Mounts...)
	}

	envFile, err := writeDockerEnvFile(requestEnv(cfg))
	if err != nil {
		cleanup()
		return nil, err
	}
	cleanups = append(cleanups, func() {
		_ = os.Remove(envFile)
	})

	args := []string{
		"run",
		"--rm",
		"-i",
		"--init",
		"--cap-drop", "ALL",
		"--cap-add", "DAC_OVERRIDE",
		"--security-opt", "no-new-privileges:true",
		"--label", fmt.Sprintf("%s=%s", dockerTaskLabelKey, dockerLabelValue(requestTaskID(cfg))),
		"--label", fmt.Sprintf("%s=%s", dockerSessionLabelKey, dockerLabelValue(cfg.SessionID)),
		"--name", dockerContainerName(cfg.SessionID),
		"--workdir", "/workspace",
		"--env-file", envFile,
	}
	if cfg.PTY || (req != nil && req.PTY) {
		args = append(args, "-t")
	}
	for _, mount := range mounts {
		args = append(args, "-v", formatDockerMount(mount))
	}

	network := r.cfg.Network
	if req != nil && req.Container != nil {
		if req.Container.Memory != "" {
			args = append(args, "--memory", req.Container.Memory)
		}
		if req.Container.CPUs > 0 {
			args = append(args, "--cpus", strconv.FormatFloat(req.Container.CPUs, 'f', -1, 64))
		}
		if req.Container.Network != "" {
			network = req.Container.Network
		}
		for _, opt := range req.Container.SecurityOpt {
			args = append(args, "--security-opt", opt)
		}
	}
	if network != "" {
		args = append(args, "--network", network)
	}

	args = append(args, r.cfg.ExtraArgs...)
	args = append(args, image)
	args = append(args, cfg.Cmd...)

	return &dockerRunSpec{
		args:    args,
		cleanup: cleanup,
	}, nil
}

func requestEnv(cfg SpawnConfig) map[string]string {
	if cfg.Request != nil {
		return cfg.Request.Env
	}
	return cfg.Env
}

func requestTaskID(cfg SpawnConfig) string {
	if cfg.TaskID != "" {
		return cfg.TaskID
	}
	if cfg.Request != nil {
		return cfg.Request.TaskID
	}
	return ""
}

func requestMounts(cfg SpawnConfig) []apischema.Mount {
	if cfg.Request != nil {
		return append([]apischema.Mount(nil), cfg.Request.EffectiveMounts()...)
	}
	if cfg.WorkDir == "" {
		return nil
	}
	return []apischema.Mount{{
		Host:      cfg.WorkDir,
		Container: "/workspace",
		Mode:      "rw",
	}}
}

func formatDockerMount(mount apischema.Mount) string {
	mode := mount.Mode
	if mode == "" {
		mode = "rw"
	}
	return fmt.Sprintf("%s:%s:%s", mount.Host, mount.Container, mode)
}

// writeDockerEnvFile writes ONLY the explicit env vars to a temp file.
// Docker containers get a clean-room environment — no parent env inheritance.
// This is the Docker isolation contract: only what the caller provides.
func writeDockerEnvFile(envMap map[string]string) (string, error) {
	if err := validateDockerEnvValues(envMap); err != nil {
		return "", err
	}

	// Build KEY=VALUE lines from only the explicit env map.
	// Do NOT call buildSpawnEnv here — that merges parent env, which is
	// correct for local runtime but wrong for Docker's clean-room model.
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		if err := validateEnvKey(k); err != nil {
			return "", fmt.Errorf("invalid env key %q: %w", k, err)
		}
		if _, reserved := reservedEnvKeys[k]; reserved {
			return "", fmt.Errorf("env key %q is reserved", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+envMap[k])
	}

	file, err := os.CreateTemp("", "agentruntime-env-")
	if err != nil {
		return "", err
	}

	if err := file.Chmod(0o600); err != nil {
		file.Close()
		_ = os.Remove(file.Name())
		return "", err
	}

	contents := strings.Join(env, "\n")
	if contents != "" {
		contents += "\n"
	}
	if _, err := file.WriteString(contents); err != nil {
		file.Close()
		_ = os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}

	return file.Name(), nil
}

func validateDockerEnvValues(envMap map[string]string) error {
	if len(envMap) == 0 {
		return nil
	}

	keys := make([]string, 0, len(envMap))
	for key := range envMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := envMap[key]
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("invalid env value for %q: must not contain newlines", key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("invalid env value for %q: must not contain NUL", key)
		}
	}

	return nil
}

func dockerContainerName(sessionID string) string {
	prefix := sessionIDPrefix(sessionID)
	if prefix == "" {
		prefix = "unknown"
	}
	return "agentruntime-" + prefix
}

func dockerLabelValue(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func sessionIDPrefix(sessionID string) string {
	if len(sessionID) <= 8 {
		return sessionID
	}
	return sessionID[:8]
}

// Recover finds running containers with the agentruntime label and returns
// handles to them. This enables session recovery after daemon restarts.
func (r *DockerRuntime) Recover(ctx context.Context) ([]ProcessHandle, error) {
	// List running containers with our label.
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q",
		"--filter", fmt.Sprintf("label=%s", dockerSessionLabelKey),
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
		labels, err := dockerContainerLabels(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("docker inspect %s: %w", id, err)
		}
		// Attach to the running container's logs for stdout.
		// This is a best-effort recovery — full stdio reattach would need docker attach.
		handles = append(handles, &recoveredDockerHandle{
			containerID: id,
			SessionID:   strings.TrimSpace(labels[dockerSessionLabelKey]),
			TaskID:      strings.TrimSpace(labels[dockerTaskLabelKey]),
		})
	}
	return handles, nil
}

func dockerContainerLabels(ctx context.Context, containerID string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .Config.Labels}}", containerID).Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	var labels map[string]string
	if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
		return nil, fmt.Errorf("parse labels: %w", err)
	}
	return labels, nil
}

// dockerHandle wraps a docker run subprocess.
type dockerHandle struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	done   chan ExitResult
}

func (h *dockerHandle) Stdin() io.WriteCloser   { return h.stdin }
func (h *dockerHandle) Stdout() io.ReadCloser   { return h.stdout }
func (h *dockerHandle) Stderr() io.ReadCloser   { return h.stderr }
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

func (h *dockerHandle) RecoveryInfo() *RecoveryInfo { return nil }

// recoveredDockerHandle is a minimal handle for containers found during recovery.
// It can kill the container but doesn't have full stdio access.
type recoveredDockerHandle struct {
	containerID string
	SessionID   string
	TaskID      string
}

func (h *recoveredDockerHandle) Stdin() io.WriteCloser { return nil }
func (h *recoveredDockerHandle) Stdout() io.ReadCloser { return nil }
func (h *recoveredDockerHandle) Stderr() io.ReadCloser { return nil }
func (h *recoveredDockerHandle) PID() int              { return 0 }
func (h *recoveredDockerHandle) RecoveryInfo() *RecoveryInfo {
	if h.SessionID == "" && h.TaskID == "" {
		return nil
	}
	return &RecoveryInfo{
		SessionID: h.SessionID,
		TaskID:    h.TaskID,
	}
}

func (h *recoveredDockerHandle) Wait() <-chan ExitResult {
	// Recovered containers need explicit management.
	ch := make(chan ExitResult)
	return ch
}

func (h *recoveredDockerHandle) Kill() error {
	return exec.Command("docker", "kill", h.containerID).Run()
}
