package runtime

import (
	"context"
	"fmt"
)

// DockerRuntime spawns agent processes inside Docker containers.
// TODO: Implement using Docker SDK (github.com/docker/docker/client).
type DockerRuntime struct {
	// Image is the default container image to use.
	Image string
}

func (r *DockerRuntime) Name() string { return "docker" }

func (r *DockerRuntime) Spawn(_ context.Context, _ SpawnConfig) (ProcessHandle, error) {
	return nil, fmt.Errorf("docker runtime not yet implemented")
}

func (r *DockerRuntime) Recover(_ context.Context) ([]ProcessHandle, error) {
	// TODO: list running containers with agentruntime labels, return handles.
	return nil, nil
}
