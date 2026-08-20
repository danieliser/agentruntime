package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/runtime"
)

func TestDockerRestrictedCodexStartsNoAmbientPluginProcesses(t *testing.T) {
	if os.Getenv("AGENTRUNTIME_DOCKER_INTEGRATION") != "1" {
		t.Skip("set AGENTRUNTIME_DOCKER_INTEGRATION=1 to run Docker qualification tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	for _, image := range []string{runtime.DefaultDockerImage, "agentruntime-proxy:latest"} {
		if output, err := exec.Command("docker", "image", "inspect", image).CombinedOutput(); err != nil {
			t.Skipf("required image %q is unavailable: %v: %s", image, err, output)
		}
	}

	request := SessionRequest{
		Agent: "codex", Runtime: "docker", Context: "clean", AutoDiscover: false,
		ExecutionPolicy: &ExecutionPolicy{
			Version: ExecutionPolicyVersion, Workspace: "ephemeral", WorkspaceRetention: "terminal_receipt",
			Filesystem: "read_only", Network: "public_https", AllowedTools: []string{}, EgressAllowlist: []string{},
			MCPServers: []string{}, HostMounts: []string{}, ApprovalPolicy: "never",
		},
	}
	resolvedPolicy, err := resolveExecutionPolicy(&request, "docker")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveNativeExecution(request, agent.DefaultRegistry().Get("codex"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	joinedCommand := strings.Join(resolved.Command, " ")
	for _, disabled := range []string{"--disable plugins", "--disable enable_mcp_apps"} {
		if !strings.Contains(joinedCommand, disabled) {
			t.Fatalf("restricted Codex command does not contain %q: %s", disabled, joinedCommand)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sessionID := uuid.NewString()
	dockerRuntime := runtime.NewDockerRuntime(runtime.DockerConfig{DataDir: t.TempDir()})
	handle, err := dockerRuntime.Spawn(ctx, runtime.SpawnConfig{
		SessionID: sessionID, Generation: 1, IdempotencyKey: "no-ambient-plugins-" + sessionID,
		RequestHash: "sha256:no-ambient-plugins", ExecutionPolicyHash: resolvedPolicy.Hash,
		AgentName: "codex", Cmd: resolved.Command, Request: &request,
	})
	if err != nil {
		t.Fatalf("start restricted Codex container: %v", err)
	}
	t.Cleanup(func() {
		_ = handle.Kill()
		_ = dockerRuntime.ReleaseSession(context.Background(), sessionID)
	})
	identified, ok := handle.(runtime.RuntimeIdentifiedHandle)
	if !ok || identified.RuntimeID() == "" {
		t.Fatal("restricted Codex handle has no container identity")
	}

	var processes string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		output, inspectErr := exec.Command("docker", "exec", identified.RuntimeID(), "sh", "-c",
			`for f in /proc/[0-9]*/cmdline; do tr '\000' ' ' < "$f" 2>/dev/null || true; printf '\n'; done`).CombinedOutput()
		if inspectErr == nil {
			processes = string(output)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processes == "" {
		t.Fatal("could not inspect restricted Codex container processes")
	}
	for _, forbidden := range []string{"codex_apps", "mcp-server", "mcp_server"} {
		if strings.Contains(strings.ToLower(processes), forbidden) {
			t.Fatalf("ambient plugin process %q exists in restricted container:\n%s", forbidden, processes)
		}
	}
	if !strings.Contains(processes, "codex app-server") {
		t.Fatalf("Codex app-server was not present during process inspection:\n%s", processes)
	}

	if err := handle.Kill(); err != nil && !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("stop restricted Codex container: %v", err)
	}
	select {
	case <-handle.Wait():
	case <-time.After(10 * time.Second):
		t.Fatal(fmt.Sprintf("restricted Codex container %s did not stop", identified.RuntimeID()))
	}
}
