package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	defaultDockerNetworkName = "agentruntime-agents"
	defaultDockerProxyImage  = "agentruntime-proxy:latest"
	dockerProxyContainerName = "agentruntime-proxy"
	dockerProxyPort          = "3128"
)

// NetworkManager owns the Docker bridge network and Squid proxy sidecar used
// to isolate agent container egress.
type NetworkManager struct {
	NetworkName string
	ProxyImage  string
	DockerHost  string // DOCKER_HOST for remote Docker daemon

	ensureOnce sync.Once
	ensureErr  error
}

func (m *NetworkManager) networkName() string {
	if m != nil && m.NetworkName != "" {
		return m.NetworkName
	}
	return defaultDockerNetworkName
}

func (m *NetworkManager) dockerHost() string {
	if m != nil {
		return m.DockerHost
	}
	return ""
}

func (m *NetworkManager) proxyImage() string {
	if m != nil && m.ProxyImage != "" {
		return m.ProxyImage
	}
	return defaultDockerProxyImage
}

func (m *NetworkManager) proxyURL() string {
	return "http://" + dockerProxyContainerName + ":" + dockerProxyPort
}

// EnsureNetwork creates the agent bridge network if it does not already exist.
// Safe for concurrent callers — "already exists" is treated as success.
func (m *NetworkManager) EnsureNetwork(ctx context.Context) error {
	if dockerNetworkExists(ctx, m.dockerHost(), m.networkName()) {
		return nil
	}
	if _, err := dockerOutputHost(ctx, m.dockerHost(), "network", "create", m.networkName()); err != nil {
		// Race: another goroutine (or prior daemon) already created it.
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("create docker network %q: %w", m.networkName(), err)
	}
	return nil
}

// EnsureProxy starts the proxy sidecar if it is not already running.
// Uses sync.Once to prevent concurrent callers from racing on container creation.
func (m *NetworkManager) EnsureProxy(ctx context.Context) error {
	m.ensureOnce.Do(func() {
		m.ensureErr = m.ensureProxyOnce(ctx)
	})
	return m.ensureErr
}

func (m *NetworkManager) ensureProxyOnce(ctx context.Context) error {
	if err := m.EnsureNetwork(ctx); err != nil {
		return err
	}

	host := m.dockerHost()
	state, err := dockerInspectRunningHost(ctx, host, dockerProxyContainerName)
	if err == nil {
		if state {
			return nil
		}
		if err := dockerRemoveContainerHost(ctx, host, dockerProxyContainerName); err != nil {
			return err
		}
	} else if !dockerObjectMissing(err) {
		return fmt.Errorf("inspect docker proxy %q: %w", dockerProxyContainerName, err)
	}

	if _, err := dockerOutputHost(
		ctx,
		host,
		"run",
		"-d",
		"--name", dockerProxyContainerName,
		"--network", m.networkName(),
		m.proxyImage(),
	); err != nil {
		// Race: proxy already started by another process.
		if strings.Contains(err.Error(), "already in use") {
			return nil
		}
		return fmt.Errorf("start docker proxy %q: %w", dockerProxyContainerName, err)
	}

	return nil
}

// ProxyEnv returns the egress proxy variables for agent containers.
func (m *NetworkManager) ProxyEnv() map[string]string {
	url := m.proxyURL()
	return map[string]string{
		"HTTP_PROXY":  url,
		"HTTPS_PROXY": url,
		"NO_PROXY":    "localhost,127.0.0.1,host.docker.internal",
	}
}

// Cleanup stops the proxy sidecar and removes the managed network.
// Resets the ensure-once gate so EnsureProxy can be called again after cleanup.
func (m *NetworkManager) Cleanup(ctx context.Context) error {
	m.ensureOnce = sync.Once{}
	m.ensureErr = nil
	var errs []error

	host := m.dockerHost()
	if exists, _ := dockerContainerExistsHost(ctx, host, dockerProxyContainerName); exists {
		if _, err := dockerOutputHost(ctx, host, "stop", dockerProxyContainerName); err != nil && !dockerObjectMissing(err) {
			errs = append(errs, fmt.Errorf("stop docker proxy %q: %w", dockerProxyContainerName, err))
		}
		if err := dockerRemoveContainerHost(ctx, host, dockerProxyContainerName); err != nil && !dockerObjectMissing(err) {
			errs = append(errs, err)
		}
	}

	if dockerNetworkExists(ctx, host, m.networkName()) {
		if _, err := dockerOutputHost(ctx, host, "network", "rm", m.networkName()); err != nil && !dockerObjectMissing(err) {
			errs = append(errs, fmt.Errorf("remove docker network %q: %w", m.networkName(), err))
		}
	}

	return errors.Join(errs...)
}

func dockerNetworkExists(ctx context.Context, host, name string) bool {
	cmd := exec.CommandContext(ctx, "docker", "network", "inspect", name)
	if host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
	}
	return cmd.Run() == nil
}

func dockerContainerExistsHost(ctx context.Context, host, name string) (bool, error) {
	if _, err := dockerOutputHost(ctx, host, "inspect", "--type", "container", name); err != nil {
		if dockerObjectMissing(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func dockerContainerExists(ctx context.Context, name string) (bool, error) {
	return dockerContainerExistsHost(ctx, "", name)
}

func dockerInspectRunningHost(ctx context.Context, host, name string) (bool, error) {
	out, err := dockerOutputHost(ctx, host, "inspect", "--type", "container", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

func dockerRemoveContainerHost(ctx context.Context, host, name string) error {
	if _, err := dockerOutputHost(ctx, host, "rm", "-f", name); err != nil {
		return fmt.Errorf("remove docker container %q: %w", name, err)
	}
	return nil
}

func dockerRemoveContainer(ctx context.Context, name string) error {
	return dockerRemoveContainerHost(ctx, "", name)
}

func dockerInspectRunning(ctx context.Context, name string) (bool, error) {
	return dockerInspectRunningHost(ctx, "", name)
}

func dockerOutput(ctx context.Context, args ...string) (string, error) {
	return dockerOutputHost(ctx, "", args...)
}

func dockerOutputHost(ctx context.Context, host string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", dockerCommandError(err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func dockerObjectMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such container") ||
		strings.Contains(msg, "No such object") ||
		strings.Contains(msg, "network not found") ||
		strings.Contains(msg, "Error: No such network")
}
