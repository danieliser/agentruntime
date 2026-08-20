package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

// TestDockerRestrictedResourceCeilings proves the admitted policy limits are
// installed on a real session container. It is opt-in with the rest of the
// Docker qualification suite.
func TestDockerRestrictedResourceCeilings(t *testing.T) {
	if os.Getenv("AGENTRUNTIME_DOCKER_INTEGRATION") != "1" {
		t.Skip("set AGENTRUNTIME_DOCKER_INTEGRATION=1 to run Docker qualification tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	image := os.Getenv("AGENTRUNTIME_DOCKER_POLICY_IMAGE")
	if image == "" {
		image = DefaultDockerImage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, requiredImage := range []string{image, defaultDockerProxyImage} {
		if _, err := dockerOutput(ctx, "image", "inspect", requiredImage); err != nil {
			t.Skipf("Docker policy image %q is unavailable: %v", requiredImage, err)
		}
	}

	sessionID := uuid.NewString()
	rt := NewDockerRuntime(DockerConfig{Image: image, DataDir: t.TempDir()})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = rt.ReleaseSession(cleanupCtx, sessionID)
	})
	handle, err := rt.Spawn(ctx, SpawnConfig{
		SessionID: sessionID, Generation: 1, IdempotencyKey: "resource-probe-" + sessionID,
		RequestHash: "sha256:resource-probe", AgentName: "resource-probe",
		Cmd: []string{"sh", "-c", "printf ready; sleep 5"},
		Request: &apischema.SessionRequest{ExecutionPolicy: &apischema.ExecutionPolicy{
			Version: "2.1", Workspace: "ephemeral", WorkspaceRetention: "terminal_receipt",
			Filesystem: "read_only", Network: "public_https", ApprovalPolicy: "never",
			EgressAllowlist: []string{}, MCPServers: []string{}, HostMounts: []string{},
			Resources: &apischema.ResourceLimits{MemoryBytes: 536870912, CPUCores: 0.5, PIDs: 64, OpenFiles: 256},
		}},
	})
	if err != nil {
		t.Fatalf("spawn resource qualification container: %v", err)
	}
	identified, ok := handle.(RuntimeIdentifiedHandle)
	if !ok {
		t.Fatal("Docker handle does not expose its container ID")
	}
	raw, err := dockerOutput(ctx, "inspect", "--format", "{{json .HostConfig}}", identified.RuntimeID())
	if err != nil {
		t.Fatalf("inspect resource qualification container: %v", err)
	}
	var hostConfig struct {
		Memory    int64 `json:"Memory"`
		NanoCPUs  int64 `json:"NanoCpus"`
		PidsLimit int64 `json:"PidsLimit"`
		Ulimits   []struct {
			Name string `json:"Name"`
			Soft int64  `json:"Soft"`
			Hard int64  `json:"Hard"`
		} `json:"Ulimits"`
	}
	if err := json.Unmarshal([]byte(raw), &hostConfig); err != nil {
		t.Fatalf("decode Docker HostConfig: %v", err)
	}
	if hostConfig.Memory != 536870912 || hostConfig.NanoCPUs != 500000000 || hostConfig.PidsLimit != 64 {
		t.Fatalf("Docker resource config = %+v", hostConfig)
	}
	foundOpenFiles := false
	for _, limit := range hostConfig.Ulimits {
		if limit.Name == "nofile" && limit.Soft == 256 && limit.Hard == 256 {
			foundOpenFiles = true
		}
	}
	if !foundOpenFiles {
		t.Fatalf("Docker nofile limit missing from %+v", hostConfig.Ulimits)
	}
	go func() { _, _ = io.Copy(io.Discard, handle.Stdout()) }()
	go func() { _, _ = io.Copy(io.Discard, handle.Stderr()) }()
	if result := <-handle.Wait(); result.Err != nil || result.Code != 0 {
		t.Fatalf("resource qualification exit = %+v", result)
	}
}

func TestDockerRestrictedMemoryBreachIsTyped(t *testing.T) {
	if os.Getenv("AGENTRUNTIME_DOCKER_INTEGRATION") != "1" {
		t.Skip("set AGENTRUNTIME_DOCKER_INTEGRATION=1 to run Docker qualification tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	image := os.Getenv("AGENTRUNTIME_DOCKER_POLICY_IMAGE")
	if image == "" {
		image = DefaultDockerImage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, requiredImage := range []string{image, defaultDockerProxyImage} {
		if _, err := dockerOutput(ctx, "image", "inspect", requiredImage); err != nil {
			t.Skipf("Docker policy image %q is unavailable: %v", requiredImage, err)
		}
	}

	sessionID := uuid.NewString()
	rt := NewDockerRuntime(DockerConfig{Image: image, DataDir: t.TempDir()})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = rt.ReleaseSession(cleanupCtx, sessionID)
	})
	handle, err := rt.Spawn(ctx, SpawnConfig{
		SessionID: sessionID, Generation: 1, IdempotencyKey: "memory-breach-" + sessionID,
		RequestHash: "sha256:memory-breach", AgentName: "resource-probe",
		Cmd: []string{"python3", "-c", "x = bytearray(256 * 1024 * 1024); print(len(x))"},
		Request: &apischema.SessionRequest{ExecutionPolicy: &apischema.ExecutionPolicy{
			Version: "2.1", Workspace: "ephemeral", WorkspaceRetention: "terminal_receipt",
			Filesystem: "read_only", Network: "public_https", ApprovalPolicy: "never",
			EgressAllowlist: []string{}, MCPServers: []string{}, HostMounts: []string{},
			Resources: &apischema.ResourceLimits{MemoryBytes: 67108864, CPUCores: 0.5, PIDs: 64, OpenFiles: 256},
		}},
	})
	if err != nil {
		t.Fatalf("spawn memory breach qualification container: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, handle.Stdout()) }()
	go func() { _, _ = io.Copy(io.Discard, handle.Stderr()) }()
	result := <-handle.Wait()
	if result.Err != nil || !result.OOMKilled || result.Code != 137 || result.Signal != "SIGKILL" || result.FailureReason != "resource_limit_exceeded" {
		t.Fatalf("memory breach terminal proof = %+v", result)
	}
}
