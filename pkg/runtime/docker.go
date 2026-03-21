package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/materialize"
)

const DefaultDockerImage = "agentruntime-agent:latest"

const (
	dockerSidecarContainerPort = "9090"
	dockerSidecarHealthPath    = "/health"
	dockerSidecarHealthTimeout = 15 * time.Second
	dockerSidecarHealthPoll    = 200 * time.Millisecond
)

// DockerConfig holds configuration for the Docker runtime.
type DockerConfig struct {
	// Image is the default container image (e.g., "agentruntime-agent:latest").
	Image string

	// Network is the Docker network to attach containers to.
	Network string

	// DataDir is the persistent agentruntime data directory for session homes.
	DataDir string

	// Host is the Docker daemon address. When set, all docker CLI commands
	// run with DOCKER_HOST=<value>. Supports ssh:// and tcp:// schemes.
	// Examples: "ssh://deploy@prod-1", "tcp://192.168.1.10:2376".
	// Empty means use the local Docker daemon (default).
	Host string

	// ExtraArgs are additional arguments passed to docker run.
	ExtraArgs []string
}

// DockerRuntime spawns agent processes inside Docker containers using the
// docker CLI. Containers are labeled with task/session identifiers for recovery.
type DockerRuntime struct {
	cfg            DockerConfig
	materializer   dockerMaterializer
	networkManager *NetworkManager
}

// NewDockerRuntime creates a new Docker runtime with the given configuration.
func NewDockerRuntime(cfg DockerConfig) *DockerRuntime {
	if cfg.Image == "" {
		cfg.Image = DefaultDockerImage
	}
	return &DockerRuntime{
		cfg: cfg,
		materializer: dockerMaterializerFunc(func(req *apischema.SessionRequest, sessionID string) (*materialize.Result, error) {
			return materialize.Materialize(req, sessionID, cfg.DataDir)
		}),
		networkManager: &NetworkManager{
			NetworkName: cfg.Network,
			DockerHost:  cfg.Host,
		},
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

// dockerCmd returns an exec.Cmd for "docker <args>" with DOCKER_HOST set if configured.
func (r *DockerRuntime) dockerCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if r.cfg.Host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+r.cfg.Host)
	}
	return cmd
}

func (r *DockerRuntime) Cleanup(ctx context.Context) error {
	return r.manager().Cleanup(ctx)
}

func (r *DockerRuntime) manager() *NetworkManager {
	if r.networkManager == nil {
		r.networkManager = &NetworkManager{NetworkName: r.cfg.Network}
	}
	return r.networkManager
}

// Spawn runs a command inside a Docker container sidecar and connects to it
// over the in-container WebSocket bridge.
func (r *DockerRuntime) Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error) {
	if len(cfg.Cmd) == 0 {
		return nil, &SpawnError{Reason: "cmd is empty"}
	}
	if err := r.manager().EnsureNetwork(ctx); err != nil {
		return nil, &SpawnError{Reason: "docker network", Err: err}
	}
	if err := r.manager().EnsureProxy(ctx); err != nil {
		return nil, &SpawnError{Reason: "docker proxy", Err: err}
	}
	spec, err := r.prepareRun(cfg)
	if err != nil {
		return nil, &SpawnError{Reason: "docker run args", Err: err}
	}

	cmd := r.dockerCmd(ctx, spec.args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "docker run", Err: dockerCommandError(err, stderr.String())}
	}

	containerID := strings.TrimSpace(stdout.String())
	if containerID == "" {
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "docker run", Err: fmt.Errorf("missing container ID")}
	}

	hostPort, err := dockerContainerPortHost(ctx, r.cfg.Host, containerID, dockerSidecarContainerPort)
	if err != nil {
		stopDockerContainerHost(r.cfg.Host, containerID)
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "docker port", Err: err}
	}

	if err := waitForDockerSidecarHealth(ctx, hostPort); err != nil {
		stopDockerContainerHost(r.cfg.Host, containerID)
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "sidecar health", Err: err}
	}

	handle, err := dialSidecar(containerID, hostPort, 0, dockerPrompt(cfg))
	if err != nil {
		stopDockerContainerHost(r.cfg.Host, containerID)
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "sidecar ws", Err: err}
	}
	handle.dockerHost = r.cfg.Host
	handle.setCleanup(spec.cleanup)
	return handle, nil
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

// dockerVolumeName returns the Docker volume name for a session.
func dockerVolumeName(sessionID string) string {
	return "agentruntime-vol-" + sessionID
}

// createSessionVolume creates a named Docker volume for session persistence.
// It is idempotent — if the volume already exists, it does not fail.
func (r *DockerRuntime) createSessionVolume(ctx context.Context, sessionID string) (string, error) {
	volumeName := dockerVolumeName(sessionID)
	cmd := r.dockerCmd(ctx,
		"volume", "create",
		"--label", fmt.Sprintf("agentruntime.session_id=%s", sessionID),
		volumeName,
	)
	// Run the command but ignore "already exists" errors
	// Docker returns a non-zero exit if the volume exists, but we treat that as success
	if err := cmd.Run(); err != nil {
		// Check if the error indicates the volume already exists
		// This is a heuristic based on docker error messages
		errStr := err.Error()
		if !strings.Contains(errStr, "already exists") && !strings.Contains(errStr, "duplicates") {
			return "", fmt.Errorf("docker volume create failed: %w", err)
		}
		// Volume already exists — that's fine for resume scenarios
	}
	return volumeName, nil
}

// RemoveSessionVolume removes a named Docker volume.
func (r *DockerRuntime) RemoveSessionVolume(ctx context.Context, volumeName string) error {
	cmd := r.dockerCmd(ctx, "volume", "rm", volumeName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker volume rm failed: %w", err)
	}
	return nil
}

// initVolumePermissions runs a short-lived container as root to chown volume
// mount points so the non-root agent user can write to them. Only processes
// volume-type mounts; bind mounts already have host-side permissions.
// Idempotent — safe to call on volumes that are already correctly owned.
func (r *DockerRuntime) initVolumePermissions(ctx context.Context, image string, mounts []apischema.Mount) error {
	var volumeMounts []apischema.Mount
	for _, m := range mounts {
		if m.Type == "volume" {
			volumeMounts = append(volumeMounts, m)
		}
	}
	if len(volumeMounts) == 0 {
		return nil
	}

	// Build a single init container that mounts all volumes at /mnt/0, /mnt/1, ...
	// and chowns them to agent:agent in one pass.
	args := []string{
		"run", "--rm", "--user", "root",
		"--entrypoint", "sh",
	}
	var chownPaths []string
	for i, m := range volumeMounts {
		mountPoint := fmt.Sprintf("/mnt/%d", i)
		args = append(args, "-v", fmt.Sprintf("%s:%s:rw", m.Host, mountPoint))
		chownPaths = append(chownPaths, mountPoint)
	}
	args = append(args, image, "-c", "chown agent:agent "+strings.Join(chownPaths, " "))

	cmd := r.dockerCmd(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("init chown failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
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

	// Validate all host mount paths (skip volume mounts)
	for _, mount := range mounts {
		if mount.Host != "" && mount.Type != "volume" {
			if err := validateMountPath(mount.Host); err != nil {
				return nil, err
			}
		}
	}

	var volumeName string
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

		// Validate materialized mounts (skip volume mounts)
		for _, mount := range result.Mounts {
			if mount.Host != "" && mount.Type != "volume" {
				if err := validateMountPath(mount.Host); err != nil {
					cleanup()
					return nil, err
				}
			}
		}

		// Create named volume for session persistence if requested
		if req.PersistSession {
			var err error
			// Use provided volume name (for resume) or create a new one
			if cfg.VolumeName != "" {
				volumeName = cfg.VolumeName
			} else {
				volumeName, err = r.createSessionVolume(context.Background(), cfg.SessionID)
				if err != nil {
					cleanup()
					return nil, err
				}
				// Register volume cleanup on failure (only for newly created volumes)
				cleanups = append(cleanups, func() {
					_ = r.RemoveSessionVolume(context.Background(), volumeName)
				})
			}
			// Add volume mount for Claude's project cache
			mounts = append(mounts, apischema.Mount{
				Host:      volumeName,
				Container: "/home/agent/.claude/projects",
				Mode:      "rw",
				Type:      "volume",
			})
		}
	}

	// Fix ownership on volume mounts so the non-root container user can write.
	// Docker volumes are root-owned by default; the agent user (UID 1000)
	// can't write to them without DAC_OVERRIDE (which requires root or
	// ambient capabilities, neither of which non-root containers have).
	if err := r.initVolumePermissions(context.Background(), image, mounts); err != nil {
		cleanup()
		return nil, fmt.Errorf("volume permission init: %w", err)
	}

	agentCmd, err := json.Marshal([]string{cfg.Cmd[0]})
	if err != nil {
		cleanup()
		return nil, err
	}

	envValues := make(map[string]string, len(requestEnv(cfg))+3)
	for key, value := range requestEnv(cfg) {
		envValues[key] = value
	}
	for key, value := range r.manager().ProxyEnv() {
		envValues[key] = value
	}
	envValues["AGENT_CMD"] = string(agentCmd)
	if acJSON := buildAgentConfigJSON(cfg); acJSON != "" {
		envValues["AGENT_CONFIG"] = acJSON
	}
	// Pass prompt via env so the sidecar knows this is fire-and-forget mode.
	// Without this, the sidecar defaults to interactive (no -p flag) and
	// Claude Code stays alive after emitting its result.
	// Base64-encoded: Docker rejects env vars containing newlines.
	interactive := cfg.Request != nil && cfg.Request.Interactive
	if prompt := dockerPrompt(cfg); prompt != "" && !interactive {
		envValues["AGENT_PROMPT"] = base64.StdEncoding.EncodeToString([]byte(prompt))
	}

	envFile, err := writeDockerEnvFile(envValues)
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
		"-d",
		"-p", "0:" + dockerSidecarContainerPort,
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
		// For named volume mounts, skip host path preparation.
		// For bind-mounts, ensure single-file mount sources exist on the host before docker run.
		// If Docker encounters a host path that doesn't exist, it creates a
		// directory at that path — which breaks file bind-mounts (e.g.
		// .claude.json, credentials). Pre-creating the file prevents this.
		if mount.Type != "volume" {
			ensureHostMountSource(mount.Host)
		}
		args = append(args, "-v", formatDockerMount(mount))
	}

	network := r.manager().networkName()
	if req != nil && req.Container != nil {
		if req.Container.Memory != "" {
			args = append(args, "--memory", req.Container.Memory)
		}
		if req.Container.CPUs > 0 {
			args = append(args, "--cpus", strconv.FormatFloat(req.Container.CPUs, 'f', -1, 64))
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

// ensureHostMountSource pre-creates the host path if it doesn't exist.
// For paths that look like files (have an extension), creates an empty file.
// For paths that look like directories, creates the directory tree.
// This prevents Docker from creating a directory when a file mount was intended.
func ensureHostMountSource(hostPath string) {
	if hostPath == "" {
		return
	}
	if _, err := os.Stat(hostPath); err == nil {
		return // already exists
	}

	// Heuristic: paths with a file extension are files, others are directories.
	base := filepath.Base(hostPath)
	if strings.Contains(base, ".") {
		// File mount — ensure parent dir exists, then touch the file.
		_ = os.MkdirAll(filepath.Dir(hostPath), 0o755)
		f, err := os.OpenFile(hostPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
		}
	} else {
		// Directory mount.
		_ = os.MkdirAll(hostPath, 0o755)
	}
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
	psCmd := r.dockerCmd(ctx, "ps", "-q",
		"--filter", fmt.Sprintf("label=%s", dockerSessionLabelKey),
	)
	out, err := psCmd.Output()
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
		labels, err := dockerContainerLabelsHost(ctx, r.cfg.Host, id)
		if err != nil {
			return nil, fmt.Errorf("docker inspect %s: %w", id, err)
		}
		sessionID := strings.TrimSpace(labels[dockerSessionLabelKey])
		taskID := strings.TrimSpace(labels[dockerTaskLabelKey])

		if hostPort, err := dockerContainerPortHost(ctx, r.cfg.Host, id, dockerSidecarContainerPort); err == nil {
			handle, err := dialSidecar(id, hostPort, 0, "")
			if err == nil {
				handle.dockerHost = r.cfg.Host
				handle.setRecoveryInfo(&RecoveryInfo{
					SessionID: sessionID,
					TaskID:    taskID,
				})
				handles = append(handles, handle)
				continue
			}
		}

		handle, err := newRecoveredDockerHandle(ctx, r.cfg.Host, id, sessionID, taskID)
		if err != nil {
			return nil, fmt.Errorf("docker logs %s: %w", id, err)
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

func dockerCommandError(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

func dockerContainerPort(ctx context.Context, containerID, containerPort string) (string, error) {
	return dockerContainerPortHost(ctx, "", containerID, containerPort)
}

func dockerContainerPortHost(ctx context.Context, host, containerID, containerPort string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "port", containerID, containerPort)
	if host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return parseDockerPortOutput(string(out))
}

func parseDockerPortOutput(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, rhs, ok := strings.Cut(line, "->"); ok {
			line = strings.TrimSpace(rhs)
		}

		idx := strings.LastIndex(line, ":")
		if idx < 0 || idx == len(line)-1 {
			continue
		}
		port := strings.TrimSpace(line[idx+1:])
		if _, err := strconv.Atoi(port); err != nil {
			continue
		}
		return port, nil
	}
	return "", fmt.Errorf("parse docker port output %q", strings.TrimSpace(output))
}

func waitForDockerSidecarHealth(ctx context.Context, hostPort string) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, dockerSidecarHealthTimeout)
	defer cancel()

	client := &http.Client{Timeout: time.Second}
	url := "http://localhost:" + hostPort + dockerSidecarHealthPath
	ticker := time.NewTicker(dockerSidecarHealthPoll)
	defer ticker.Stop()

	type sidecarHealthResponse struct {
		Status      string `json:"status"`
		AgentType   string `json:"agent_type"`
		ErrorDetail string `json:"error_detail"`
	}
	var lastHTTPDetail string

	for {
		req, err := http.NewRequestWithContext(deadlineCtx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				var health sidecarHealthResponse
				decodeErr := json.NewDecoder(resp.Body).Decode(&health)
				_ = resp.Body.Close()
				if decodeErr == nil && health.Status == "error" {
					detail := strings.TrimSpace(health.ErrorDetail)
					if detail == "" {
						detail = "unknown sidecar error"
					}
					return fmt.Errorf("sidecar health check failed: %s", detail)
				}
				if decodeErr == nil && strings.TrimSpace(health.AgentType) != "" {
					return nil
				}
			} else {
				lastHTTPDetail = fmt.Sprintf("status %s: %s", resp.Status, strings.TrimSpace(httpResponseBody(resp)))
				_ = resp.Body.Close()
			}
		}

		select {
		case <-deadlineCtx.Done():
			if lastHTTPDetail != "" {
				return fmt.Errorf("timed out waiting for sidecar health on port %s: %s", hostPort, lastHTTPDetail)
			}
			return fmt.Errorf("timed out waiting for sidecar health on port %s: %w", hostPort, deadlineCtx.Err())
		case <-ticker.C:
		}
	}
}

func dockerPrompt(cfg SpawnConfig) string {
	if cfg.Prompt != "" {
		return cfg.Prompt
	}
	if len(cfg.Cmd) > 1 {
		return cfg.Cmd[len(cfg.Cmd)-1]
	}
	return ""
}

func stopDockerContainer(containerID string) {
	stopDockerContainerHost("", containerID)
}

func stopDockerContainerHost(host, containerID string) {
	if containerID == "" {
		return
	}
	_, _ = dockerOutputHost(context.Background(), host, "stop", containerID)
	_, _ = dockerOutputHost(context.Background(), host, "rm", containerID)
}

func dockerContainerLabels(ctx context.Context, containerID string) (map[string]string, error) {
	return dockerContainerLabelsHost(ctx, "", containerID)
}

func dockerContainerLabelsHost(ctx context.Context, host, containerID string) (map[string]string, error) {
	out, err := dockerOutputHost(ctx, host, "inspect", "--format", "{{json .Config.Labels}}", containerID)
	if err != nil {
		return nil, err
	}

	if out == "" || out == "null" {
		return nil, nil
	}

	var labels map[string]string
	if err := json.Unmarshal([]byte(out), &labels); err != nil {
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
// It follows docker logs so recovered sessions can resume stdout/stderr streaming.
type recoveredDockerHandle struct {
	containerID string
	dockerHost  string
	SessionID   string
	TaskID      string

	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	done   chan ExitResult
	killMu sync.Mutex
}

func (h *recoveredDockerHandle) Stdin() io.WriteCloser { return nil }
func (h *recoveredDockerHandle) Stdout() io.ReadCloser { return h.stdout }
func (h *recoveredDockerHandle) Stderr() io.ReadCloser { return h.stderr }
func (h *recoveredDockerHandle) PID() int {
	if h.cmd != nil && h.cmd.Process != nil {
		return h.cmd.Process.Pid
	}
	return 0
}
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
	return h.done
}

func (h *recoveredDockerHandle) Kill() error {
	h.killMu.Lock()
	defer h.killMu.Unlock()
	_, err := dockerOutputHost(context.Background(), h.dockerHost, "kill", h.containerID)
	return err
}

func newRecoveredDockerHandle(ctx context.Context, host, containerID, sessionID, taskID string) (*recoveredDockerHandle, error) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "--follow", "--since=0", containerID)
	if host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	handle := &recoveredDockerHandle{
		containerID: containerID,
		dockerHost:  host,
		SessionID:   sessionID,
		TaskID:      taskID,
		cmd:         cmd,
		stdout:      stdout,
		stderr:      stderr,
		done:        make(chan ExitResult, 1),
	}
	go func() {
		waitErr := cmd.Wait()
		code := 0
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
				waitErr = nil
			}
		}
		handle.done <- ExitResult{Code: code, Err: waitErr}
	}()
	return handle, nil
}

// validateMountPath validates a host mount path for security.
// It checks that the path is absolute and exists.
func validateMountPath(path string) error {
	// Quick validation: path should be absolute and exist
	// More thorough validation is done in session.ValidateWorkDir (API layer)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("invalid mount path (must be absolute): %s", path)
	}

	// Docker supports both file and directory bind-mount sources.
	// Single-file mounts are used for .claude.json, credentials, etc.
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("mount path does not exist: %s", path)
		}
		return fmt.Errorf("cannot stat mount path: %s: %v", path, err)
	}

	return nil
}
