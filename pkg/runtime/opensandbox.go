package runtime

import (
	"context"
	"fmt"
)

// OpenSandboxRuntime spawns agents via the OpenSandbox execd HTTP/WS API.
// TODO: Implement using HTTP client to execd endpoints.
type OpenSandboxRuntime struct {
	// BaseURL is the execd API base URL (e.g., "http://localhost:8080").
	BaseURL string
}

func (r *OpenSandboxRuntime) Name() string                    { return "opensandbox" }
func (r *OpenSandboxRuntime) Cleanup(_ context.Context) error { return nil }

func (r *OpenSandboxRuntime) Spawn(_ context.Context, _ SpawnConfig) (ProcessHandle, error) {
	return nil, fmt.Errorf("opensandbox runtime not yet implemented")
}

func (r *OpenSandboxRuntime) Recover(_ context.Context) ([]ProcessHandle, error) {
	// TODO: list active execd sessions, return handles.
	return nil, nil
}
