package runtime

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

type dockerBindMountAdmission struct {
	field string
	mount apischema.Mount
}

// CheckRequestAdmission asks the Docker daemon to create, but never start, a
// short-lived container for each requested bind source. Host-side os.Stat is
// insufficient for Docker Desktop and remote daemons, where an existing path
// can still be outside the daemon's shared filesystem.
func (r *DockerRuntime) CheckRequestAdmission(parent context.Context, cfg SpawnConfig) error {
	candidates := dockerBindMountAdmissions(cfg)
	if len(candidates) == 0 {
		return nil
	}
	image := resolvedDockerImage(r.cfg, cfg)
	if image == "" {
		return fmt.Errorf("Docker request admission has no configured image")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	for index, candidate := range candidates {
		if err := validateMountPath(candidate.mount.Host); err != nil {
			return &RequestAdmissionError{Field: candidate.field, Value: candidate.mount.Host, Err: err}
		}
		mountSpec := fmt.Sprintf("type=bind,%s,dst=/__agentruntime_admission/%d", dockerMountCSVField("src", candidate.mount.Host), index)
		if candidate.mount.Mode == "ro" {
			mountSpec += ",readonly"
		}
		volumeProbe := candidate.mount
		volumeProbe.Container = fmt.Sprintf("/__agentruntime_admission_volume/%d", index)
		cmd := r.dockerCmd(ctx, "create", "--network", "none", "--mount", mountSpec, "-v", formatDockerMount(volumeProbe), image)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			cause := dockerCommandError(err, stderr.String())
			if dockerBindMountRejected(candidate.mount.Host, stderr.String()) {
				return &RequestAdmissionError{Field: candidate.field, Value: candidate.mount.Host, Err: cause}
			}
			return fmt.Errorf("Docker bind-mount admission probe for %q failed: %w", candidate.mount.Host, cause)
		}
		containerID := strings.TrimSpace(stdout.String())
		if containerID == "" {
			return fmt.Errorf("Docker bind-mount admission probe for %q returned no container ID", candidate.mount.Host)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, cleanupErr := dockerOutputHost(cleanupCtx, r.cfg.Host, "rm", "-f", containerID)
		cleanupCancel()
		if cleanupErr != nil && !dockerObjectMissing(cleanupErr) {
			return fmt.Errorf("remove Docker bind-mount admission probe %q: %w", containerID, cleanupErr)
		}
	}
	return nil
}

func dockerMountCSVField(key, value string) string {
	field := key + "=" + value
	if !strings.ContainsAny(field, "\",\r\n") {
		return field
	}
	return `"` + strings.ReplaceAll(field, `"`, `""`) + `"`
}

func dockerBindMountAdmissions(cfg SpawnConfig) []dockerBindMountAdmission {
	if cfg.Request == nil {
		if cfg.WorkDir == "" {
			return nil
		}
		return []dockerBindMountAdmission{{field: "work_dir", mount: apischema.Mount{Host: cfg.WorkDir, Container: "/workspace", Mode: "rw"}}}
	}
	req := cfg.Request
	result := make([]dockerBindMountAdmission, 0, len(req.Mounts)+len(req.Volumes)+1)
	if req.WorkDir != "" {
		result = append(result, dockerBindMountAdmission{field: "work_dir", mount: apischema.Mount{Host: req.WorkDir, Container: "/workspace", Mode: "rw"}})
	}
	for index, mount := range req.Mounts {
		if mount.Type != "volume" {
			result = append(result, dockerBindMountAdmission{field: fmt.Sprintf("mounts[%d].host", index), mount: mount})
		}
	}
	for index, mount := range req.ParseVolumes() {
		result = append(result, dockerBindMountAdmission{field: fmt.Sprintf("volumes[%d]", index), mount: mount})
	}
	return result
}

func dockerBindMountRejected(path, stderr string) bool {
	lower := strings.ToLower(stderr)
	if strings.Contains(stderr, path) {
		return true
	}
	return strings.Contains(lower, "mounts denied") || strings.Contains(lower, "bind source path") ||
		strings.Contains(lower, "invalid mount config") || strings.Contains(lower, "not shared from the host")
}
