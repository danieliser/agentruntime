package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// DockerConfig holds configuration for the Docker runtime.
type DockerConfig struct {
	// Image is the default container image (e.g., "agentruntime-agent:latest").
	Image string
	// ProxyImage is the managed egress proxy image.
	ProxyImage string

	// ExpectedVersion and ExpectedCommit require exact OCI release stamps on
	// both the agent and proxy images. Development configs leave them empty.
	ExpectedVersion string
	ExpectedCommit  string

	// Network is the Docker network to attach containers to.
	Network string

	// DataDir is the persistent agentruntime data directory for session homes.
	DataDir string
	// DiagnosticDir stores private policy-egress diagnostics. Empty disables
	// the diagnostic sink even when a session requests it.
	DiagnosticDir string

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
	if cfg.ProxyImage == "" {
		cfg.ProxyImage = defaultDockerProxyImage
	}
	return &DockerRuntime{
		cfg: cfg,
		materializer: dockerMaterializerFunc(func(req *apischema.SessionRequest, sessionID string) (*materialize.Result, error) {
			return materialize.Materialize(req, sessionID, cfg.DataDir)
		}),
		networkManager: &NetworkManager{
			NetworkName:   cfg.Network,
			ProxyImage:    cfg.ProxyImage,
			DockerHost:    cfg.Host,
			DataDir:       cfg.DataDir,
			DiagnosticDir: cfg.DiagnosticDir,
		},
	}
}

const (
	dockerTaskLabelKey           = "agentruntime.task_id"
	dockerSessionLabelKey        = "agentruntime.session_id"
	dockerGenerationLabelKey     = "agentruntime.generation"
	dockerIdempotencyLabelKey    = "agentruntime.idempotency_key"
	dockerRequestHashLabelKey    = "agentruntime.request_hash"
	dockerAgentLabelKey          = "agentruntime.agent"
	dockerImageReferenceLabelKey = "agentruntime.image_reference"
	dockerImageDigestLabelKey    = "agentruntime.image_digest"
	dockerSandboxProfileLabelKey = "agentruntime.sandbox_profile"
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

// CheckAdmission verifies both Docker CLI discovery and daemon reachability
// without creating a container or otherwise mutating runtime state.
func (r *DockerRuntime) CheckAdmission(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := dockerOutputHost(checkCtx, r.cfg.Host, "ps", "-q", "--no-trunc"); err != nil {
		return fmt.Errorf("Docker runtime unavailable: %w", err)
	}
	for _, image := range []string{r.cfg.Image, r.cfg.ProxyImage} {
		if _, err := dockerOutputHost(checkCtx, r.cfg.Host, "image", "inspect", image); err != nil {
			return fmt.Errorf("Docker configured image %q is unavailable: %w", image, err)
		}
	}
	if r.cfg.ExpectedVersion != "" || r.cfg.ExpectedCommit != "" {
		for _, image := range []string{r.cfg.Image, r.cfg.ProxyImage} {
			if err := r.checkImageStamp(checkCtx, image); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *DockerRuntime) checkImageStamp(ctx context.Context, image string) error {
	raw, err := dockerOutputHost(ctx, r.cfg.Host, "image", "inspect", "--format", "{{json .Config.Labels}}", image)
	if err != nil {
		return fmt.Errorf("inspect Docker image stamp %q: %w", image, err)
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return fmt.Errorf("decode Docker image stamp %q: %w", image, err)
	}
	if got := labels["org.opencontainers.image.version"]; got != r.cfg.ExpectedVersion {
		return fmt.Errorf("Docker image %q version stamp %q does not match %q", image, got, r.cfg.ExpectedVersion)
	}
	if got := labels["org.opencontainers.image.revision"]; got != r.cfg.ExpectedCommit {
		return fmt.Errorf("Docker image %q revision stamp %q does not match %q", image, got, r.cfg.ExpectedCommit)
	}
	return nil
}

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

// ReleaseSession destroys a policy-v1 container and its provider-private
// session state after the durable terminal receipt is committed. Durable
// events, results, receipts, and chat history live outside these paths.
func (r *DockerRuntime) ReleaseSession(ctx context.Context, sessionID string) error {
	if !safeRuntimeSessionID(sessionID) {
		return fmt.Errorf("release session: unsafe session ID")
	}
	var errs []error
	output, err := dockerOutputHost(ctx, r.cfg.Host, "ps", "-aq", "--no-trunc", "--filter", fmt.Sprintf("label=%s=%s", dockerSessionLabelKey, sessionID))
	if err != nil {
		errs = append(errs, fmt.Errorf("list session containers: %w", err))
	} else {
		for _, containerID := range strings.Fields(output) {
			if _, err := dockerOutputHost(ctx, r.cfg.Host, "rm", "-f", containerID); err != nil && !dockerObjectMissing(err) {
				errs = append(errs, fmt.Errorf("remove session container %q: %w", containerID, err))
			}
		}
	}
	if r.cfg.DataDir != "" {
		for _, root := range []string{"claude-sessions", "codex-sessions"} {
			path := filepath.Join(r.cfg.DataDir, root, sessionID)
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, fmt.Errorf("remove %s state: %w", root, err))
			}
		}
	}
	if err := r.manager().ReleasePolicySession(ctx, sessionID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func safeRuntimeSessionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func (r *DockerRuntime) manager() *NetworkManager {
	if r.networkManager == nil {
		r.networkManager = &NetworkManager{NetworkName: r.cfg.Network, ProxyImage: r.cfg.ProxyImage, DockerHost: r.cfg.Host, DataDir: r.cfg.DataDir, DiagnosticDir: r.cfg.DiagnosticDir}
	}
	return r.networkManager
}

// Spawn runs a command inside a Docker container. Claude and Codex always
// expose their direct provider stdio; no execution sidecar participates.
func (r *DockerRuntime) Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error) {
	if len(cfg.Cmd) == 0 {
		return nil, &SpawnError{Reason: "cmd is empty"}
	}
	restricted := cfg.Request != nil && cfg.Request.ExecutionPolicy != nil
	if restricted {
		policyCtx, cancelPolicy := context.WithTimeout(ctx, 60*time.Second)
		defer cancelPolicy()
		policyNetwork, err := policyNetworkSpec(cfg)
		if err != nil {
			return nil, &SpawnError{Reason: "egress policy", Err: err}
		}
		if err := r.manager().EnsurePolicyProxy(policyCtx, policyNetwork); err != nil {
			return nil, &SpawnError{Reason: "policy proxy", Err: &EgressError{Code: EgressPreflightFailed, Stage: "policy proxy unavailable", Err: err}}
		}
		if err := r.manager().preflightPolicyEgress(policyCtx, policyNetwork, resolvedDockerImage(r.cfg.Image, cfg)); err != nil {
			return nil, &SpawnError{Reason: "egress preflight", Err: err}
		}
	} else {
		if err := r.manager().EnsureNetwork(ctx); err != nil {
			return nil, &SpawnError{Reason: "docker network", Err: err}
		}
		if err := r.manager().EnsureProxy(ctx); err != nil {
			return nil, &SpawnError{Reason: "docker proxy", Err: err}
		}
	}
	if cfg.Generation > 0 {
		cfg.ImageReference = resolvedDockerImage(r.cfg.Image, cfg)
		imageDigest, err := r.imageReferenceDigest(ctx, cfg.ImageReference)
		if err != nil {
			return nil, &SpawnError{Reason: "image digest", Err: err}
		}
		cfg.ImageDigest = imageDigest
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
	imageDigest := ""
	if cfg.Generation > 0 {
		imageDigest, err = r.containerImageDigest(ctx, containerID)
		if err != nil {
			stopDockerContainerHost(r.cfg.Host, containerID)
			if spec.cleanup != nil {
				spec.cleanup()
			}
			return nil, &SpawnError{Reason: "container image digest", Err: err}
		}
		if imageDigest != cfg.ImageDigest {
			stopDockerContainerHost(r.cfg.Host, containerID)
			if spec.cleanup != nil {
				spec.cleanup()
			}
			return nil, &SpawnError{Reason: "container image digest", Err: fmt.Errorf("container image %q does not match admitted digest %q", imageDigest, cfg.ImageDigest)}
		}
	}
	handle, err := newNativeDockerHandle(r.cfg.Host, containerID, RecoveryInfo{})
	if err != nil {
		stopDockerContainerHost(r.cfg.Host, containerID)
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return nil, &SpawnError{Reason: "native docker stdio", Err: err}
	}
	handle.imageDigest = imageDigest
	// Materialized files remain in place for restart reconstruction. A later
	// session-retention pass owns their eventual removal.
	return handle, nil
}

func (r *DockerRuntime) InspectEgressFailure(ctx context.Context, cfg SpawnConfig) error {
	spec, err := policyNetworkSpec(cfg)
	if err != nil {
		return err
	}
	return r.manager().inspectPolicyEgressFailure(ctx, spec)
}

func (r *DockerRuntime) imageReferenceDigest(ctx context.Context, imageReference string) (string, error) {
	var stderr bytes.Buffer
	cmd := r.dockerCmd(ctx, "image", "inspect", "--format", "{{.Id}}", imageReference)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", dockerCommandError(err, stderr.String())
	}
	return validatedSHA256Digest(out)
}

func (r *DockerRuntime) containerImageDigest(ctx context.Context, containerID string) (string, error) {
	var stderr bytes.Buffer
	cmd := r.dockerCmd(ctx, "inspect", "--format", "{{.Image}}", containerID)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", dockerCommandError(err, stderr.String())
	}
	return validatedSHA256Digest(out)
}

func validatedSHA256Digest(raw []byte) (string, error) {
	digest := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(digest, "sha256:") || len(digest) <= len("sha256:") {
		return "", fmt.Errorf("invalid image digest %q", digest)
	}
	return digest, nil
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

// CreateNamedVolume creates a named Docker volume with optional labels.
// Idempotent — does not fail if the volume already exists.
func (r *DockerRuntime) CreateNamedVolume(ctx context.Context, name string, labels map[string]string) error {
	args := []string{"volume", "create"}
	for k, v := range labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, name)
	cmd := r.dockerCmd(ctx, args...)
	if err := cmd.Run(); err != nil {
		errStr := err.Error()
		if !strings.Contains(errStr, "already exists") && !strings.Contains(errStr, "duplicates") {
			return fmt.Errorf("docker volume create %s: %w", name, err)
		}
	}
	return nil
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
	image := resolvedDockerImage(r.cfg.Image, cfg)
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
	if req != nil && (req.Agent == "claude" || req.Agent == "codex" || req.Claude != nil || req.Codex != nil) {
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

		// Create named volume for session persistence if requested.
		// Skip if a volume mount already targets the same container path
		// (e.g., the chat manager's per-chat volume). Adding a second
		// volume at the same path would shadow the first, breaking resume
		// because the JSONL from the previous session lives on the chat
		// volume while the new session reads from the per-session volume.
		if req.PersistSession && !hasVolumeMountAt(mounts, "/home/agent/.claude/projects") {
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

	// Mount the team directory for Agent Teams inbox protocol.
	// The Claude binary polls ~/.claude/teams/{name}/inboxes/{agent}.json
	// for messages — the directory must be accessible inside the container.
	if req != nil && req.Team != nil && req.Team.Name != "" {
		home, _ := os.UserHomeDir()
		teamDir := filepath.Join(home, ".claude", "teams", req.Team.Name)
		containerTeamDir := "/home/agent/.claude/teams/" + req.Team.Name
		if err := validateMountPath(teamDir); err != nil {
			cleanup()
			return nil, fmt.Errorf("team directory: %w", err)
		}
		mounts = append(mounts, apischema.Mount{
			Host:      teamDir,
			Container: containerTeamDir,
			Mode:      "rw",
		})
	}

	// Fix ownership on volume mounts so the non-root container user can write.
	// Docker volumes are root-owned by default; the agent user (UID 1000)
	// can't write to them without DAC_OVERRIDE (which requires root or
	// ambient capabilities, neither of which non-root containers have).
	if err := r.initVolumePermissions(context.Background(), image, mounts); err != nil {
		cleanup()
		return nil, fmt.Errorf("volume permission init: %w", err)
	}

	if err := validateCallerDockerEnv(requestEnv(cfg)); err != nil {
		cleanup()
		return nil, err
	}
	envValues := make(map[string]string, len(requestEnv(cfg))+8)
	for key, value := range requestEnv(cfg) {
		envValues[key] = value
	}
	restricted := req != nil && req.ExecutionPolicy != nil
	proxyEnv := r.manager().ProxyEnv()
	policyNetwork := PolicyNetworkSpec{}
	if restricted {
		var policyErr error
		policyNetwork, policyErr = policyNetworkSpec(cfg)
		if policyErr != nil {
			cleanup()
			return nil, policyErr
		}
		proxyEnv = r.manager().RestrictedProxyEnv()
	}
	for key, value := range proxyEnv {
		envValues[key] = value
	}
	// Session identity remains available to provider processes and hooks.
	if cfg.SessionID != "" {
		envValues["SESSION_ID"] = cfg.SessionID
	}
	if cfg.TaskID != "" {
		envValues["TASK_ID"] = cfg.TaskID
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
		"run", "-d", "--init",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--label", fmt.Sprintf("%s=%s", dockerTaskLabelKey, dockerLabelValue(requestTaskID(cfg))),
		"--label", fmt.Sprintf("%s=%s", dockerSessionLabelKey, dockerLabelValue(cfg.SessionID)),
		"--label", fmt.Sprintf("%s=%s", dockerAgentLabelKey, dockerLabelValue(cfg.AgentName)),
		"--name", dockerContainerName(cfg.SessionID),
		"--workdir", "/workspace",
		"--env-file", envFile,
	}
	if restricted {
		memory, cpus, pids, openFiles := "2g", "2", int64(256), int64(1024)
		if limits := req.ExecutionPolicy.Resources; limits != nil {
			memory = strconv.FormatInt(limits.MemoryBytes, 10)
			cpus = strconv.FormatFloat(limits.CPUCores, 'f', -1, 64)
			pids = limits.PIDs
			openFiles = limits.OpenFiles
		}
		args = append(args,
			"--read-only",
			"--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
			"--memory", memory,
			"--cpus", cpus,
			"--pids-limit", strconv.FormatInt(pids, 10),
			"--ulimit", fmt.Sprintf("nofile=%d:%d", openFiles, openFiles),
		)
		if req.ExecutionPolicy.Filesystem == "workspace_write" {
			args = append(args, "--tmpfs", "/workspace:rw,nosuid,nodev,size=256m")
		}
	} else {
		args = append(args, "--cap-add", "DAC_OVERRIDE")
	}
	args = append(args,
		"-i",
		"--log-driver", "json-file",
		"--entrypoint", cfg.Cmd[0],
	)
	if cfg.Generation > 0 {
		args = append(args,
			"--label", fmt.Sprintf("%s=%d", dockerGenerationLabelKey, cfg.Generation),
			"--label", fmt.Sprintf("%s=%s", dockerIdempotencyLabelKey, dockerLabelValue(cfg.IdempotencyKey)),
			"--label", fmt.Sprintf("%s=%s", dockerRequestHashLabelKey, dockerLabelValue(cfg.RequestHash)),
			"--label", fmt.Sprintf("%s=%s", dockerImageReferenceLabelKey, dockerLabelValue(image)),
			"--label", fmt.Sprintf("%s=%s", dockerImageDigestLabelKey, dockerLabelValue(cfg.ImageDigest)),
			"--label", fmt.Sprintf("%s=%s", dockerSandboxProfileLabelKey, dockerLabelValue(cfg.SandboxProfile)),
		)
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
	if restricted {
		network = policyNetwork.NetworkName()
	}
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
	args = append(args, cfg.Cmd[1:]...)

	return &dockerRunSpec{
		args:    args,
		cleanup: cleanup,
	}, nil
}

func resolvedDockerImage(defaultImage string, cfg SpawnConfig) string {
	if cfg.ImageReference != "" {
		return cfg.ImageReference
	}
	if cfg.Request != nil && cfg.Request.Container != nil && cfg.Request.Container.Image != "" {
		return cfg.Request.Container.Image
	}
	return defaultImage
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

// hasVolumeMountAt checks whether any existing mount targets the given
// container path with type "volume". Used to avoid shadow-mounting a
// per-session volume over a caller-supplied chat volume at the same path.
func hasVolumeMountAt(mounts []apischema.Mount, containerPath string) bool {
	for _, m := range mounts {
		if m.Type == "volume" && m.Container == containerPath {
			return true
		}
	}
	return false
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

func validateCallerDockerEnv(envMap map[string]string) error {
	for _, key := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	} {
		if _, exists := envMap[key]; exists {
			return fmt.Errorf("env key %q is reserved for AgentD-managed egress", key)
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

// Recover finds running and stopped containers with the agentruntime label.
// Stopped durable generations still expose retained logs and docker wait proof
// so AgentD can finish their terminal ledger after a daemon restart.
func (r *DockerRuntime) Recover(ctx context.Context) ([]ProcessHandle, error) {
	// docker run returns the canonical 64-character ID persisted with the
	// generation. Recovery must not compare that proof to ps's 12-character
	// display form.
	psCmd := r.dockerCmd(ctx, "ps", "-aq", "--no-trunc",
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
		generation := int64(0)
		if rawGeneration := strings.TrimSpace(labels[dockerGenerationLabelKey]); rawGeneration != "" {
			generation, err = strconv.ParseInt(rawGeneration, 10, 64)
			if err != nil || generation < 1 {
				return nil, fmt.Errorf("docker inspect %s: invalid durable generation label %q", id, rawGeneration)
			}
		}
		idempotencyKey := strings.TrimSpace(labels[dockerIdempotencyLabelKey])
		requestHash := strings.TrimSpace(labels[dockerRequestHashLabelKey])
		agentName := strings.TrimSpace(labels[dockerAgentLabelKey])
		recovery := RecoveryInfo{
			SessionID: sessionID, TaskID: taskID, AgentName: agentName, Generation: generation,
			IdempotencyKey: idempotencyKey, RequestHash: requestHash,
			ImageReference: strings.TrimSpace(labels[dockerImageReferenceLabelKey]),
			ImageDigest:    strings.TrimSpace(labels[dockerImageDigestLabelKey]),
			SandboxProfile: strings.TrimSpace(labels[dockerSandboxProfileLabelKey]),
		}

		handle, err := newNativeDockerHandle(r.cfg.Host, id, recovery)
		if err != nil {
			return nil, fmt.Errorf("recover native docker stdio %s: %w", id, err)
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

func (h *dockerHandle) RuntimeID() string {
	if pid := h.PID(); pid > 0 {
		return fmt.Sprintf("docker-cli-pid:%d", pid)
	}
	return ""
}

func (*dockerHandle) NativeStdio() bool { return true }

// recoveredDockerHandle is a minimal handle for containers found during recovery.
// It follows docker logs so recovered sessions can resume stdout/stderr streaming.
type recoveredDockerHandle struct {
	containerID string
	dockerHost  string
	recovery    RecoveryInfo

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
	if h.recovery.SessionID == "" && h.recovery.TaskID == "" {
		return nil
	}
	copy := h.recovery
	return &copy
}

func (h *recoveredDockerHandle) RuntimeID() string { return h.containerID }

func (*recoveredDockerHandle) NativeStdio() bool { return true }

func (h *recoveredDockerHandle) Wait() <-chan ExitResult {
	return h.done
}

func (h *recoveredDockerHandle) Kill() error {
	h.killMu.Lock()
	defer h.killMu.Unlock()
	_, err := dockerOutputHost(context.Background(), h.dockerHost, "kill", h.containerID)
	return err
}

func newRecoveredDockerHandle(ctx context.Context, host, containerID string, recovery RecoveryInfo) (*recoveredDockerHandle, error) {
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
		recovery:    recovery,
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
