package runtime

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

// TestDockerRestrictedWorkspaceQualification proves the policy-v2 boundary
// against a real Docker daemon. It is opt-in because it builds/uses the local
// AgentD agent and proxy images and makes one public HTTPS request.
func TestDockerRestrictedWorkspaceQualification(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, requiredImage := range []string{image, defaultDockerProxyImage} {
		if _, err := dockerOutput(ctx, "image", "inspect", requiredImage); err != nil {
			t.Skipf("Docker policy image %q is unavailable: %v", requiredImage, err)
		}
	}

	dataDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "host-project-secret.txt")
	if err := os.WriteFile(sentinel, []byte("must-not-be-visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.NewString()
	runtime := NewDockerRuntime(DockerConfig{Image: image, DataDir: dataDir})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = runtime.ReleaseSession(cleanupCtx, sessionID)
	})

	const probe = `set -eu
test "$(id -u)" != "0"
test -z "$(find /workspace -mindepth 1 -maxdepth 1 -print -quit)"
test ! -e "$1"
test ! -e /home/agent/.codex/auth.json
test ! -e /home/agent/.config/automem
test ! -e /home/agent/.config/mcp
test -f /home/agent/.codex/config.toml
! grep -qi 'mcp' /home/agent/.codex/config.toml
! touch /workspace/agentd-write-probe
! touch /etc/agentd-write-probe
if env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy -u ALL_PROXY -u all_proxy -u NO_PROXY -u no_proxy curl --noproxy '*' --connect-timeout 3 --max-time 5 -fsS https://api.openai.com/ >/dev/null 2>&1; then
  exit 41
fi
if env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy -u ALL_PROXY -u all_proxy -u NO_PROXY -u no_proxy curl --noproxy '*' --connect-timeout 3 --max-time 5 -kfsS https://1.1.1.1/ >/dev/null 2>&1; then
  exit 42
fi
if curl --connect-timeout 3 --max-time 5 -fsS https://example.com/ >/dev/null 2>&1; then
  exit 43
fi
status="$(curl --connect-timeout 10 --max-time 15 -sS -o /dev/null -w '%{http_code}' https://api.openai.com/)"
test "$status" != "000"
printf '%s\n' '{"qualified":true}'`
	handle, err := runtime.Spawn(ctx, SpawnConfig{
		SessionID: sessionID, Generation: 1, IdempotencyKey: "policy-qualification-" + sessionID,
		RequestHash: "sha256:policy-qualification", AgentName: "codex",
		Cmd: []string{"sh", "-c", probe, "--", sentinel},
		Request: &apischema.SessionRequest{Agent: "codex", Context: "clean", AutoDiscover: false,
			ExecutionPolicy: &apischema.ExecutionPolicy{
				Version: "2.0", Workspace: "ephemeral", WorkspaceRetention: "terminal_receipt",
				Filesystem: "read_only", Network: "public_https", AllowedTools: []string{"web_search"},
				EgressAllowlist: []string{"api.openai.com", "chatgpt.com"},
				MCPServers:      []string{}, HostMounts: []string{}, ApprovalPolicy: "never",
			},
		},
	})
	if err != nil {
		t.Fatalf("spawn restricted qualification container: %v", err)
	}
	outputResult := make(chan struct {
		bytes []byte
		err   error
	}, 1)
	go func() {
		bytes, err := io.ReadAll(handle.Stdout())
		outputResult <- struct {
			bytes []byte
			err   error
		}{bytes: bytes, err: err}
	}()
	errorResult := make(chan []byte, 1)
	go func() {
		bytes, _ := io.ReadAll(handle.Stderr())
		errorResult <- bytes
	}()
	result := <-handle.Wait()
	if err := runtime.ReleaseSession(ctx, sessionID); err != nil {
		t.Fatalf("release qualified session: %v", err)
	}
	var output []byte
	var readErr error
	select {
	case read := <-outputResult:
		output, readErr = read.bytes, read.err
	case <-time.After(10 * time.Second):
		t.Fatal("Docker log follower did not close after terminal-retention cleanup")
	}
	if readErr != nil || result.Err != nil || result.Code != 0 || !strings.Contains(string(output), `{"qualified":true}`) {
		t.Fatalf("restricted qualification output=%q stderr=%q read_err=%v exit=%+v", output, <-errorResult, readErr, result)
	}
	for _, root := range []string{"claude-sessions", "codex-sessions"} {
		if _, err := os.Stat(filepath.Join(dataDir, root, sessionID)); !os.IsNotExist(err) {
			t.Fatalf("terminal retention left %s state: %v", root, err)
		}
	}

	denySessionID := uuid.NewString()
	denyHandle, err := runtime.Spawn(ctx, SpawnConfig{
		SessionID: denySessionID, Generation: 1, IdempotencyKey: "policy-deny-all-" + denySessionID,
		RequestHash: "sha256:policy-deny-all", AgentName: "codex",
		Cmd: []string{"sh", "-c", `set -eu
if curl --connect-timeout 3 --max-time 5 -fsS https://api.openai.com/ >/dev/null 2>&1; then
  exit 51
fi
printf '%s\n' '{"deny_all":true}'`},
		Request: &apischema.SessionRequest{Agent: "codex", Context: "clean", AutoDiscover: false,
			ExecutionPolicy: &apischema.ExecutionPolicy{
				Version: "2.0", Workspace: "ephemeral", WorkspaceRetention: "terminal_receipt",
				Filesystem: "read_only", Network: "public_https", AllowedTools: []string{},
				EgressAllowlist: []string{}, MCPServers: []string{}, HostMounts: []string{}, ApprovalPolicy: "never",
			},
		},
	})
	if err != nil {
		t.Fatalf("spawn deny-all qualification container: %v", err)
	}
	denyOutput := make(chan []byte, 1)
	go func() {
		bytes, _ := io.ReadAll(denyHandle.Stdout())
		denyOutput <- bytes
	}()
	go func() { _, _ = io.Copy(io.Discard, denyHandle.Stderr()) }()
	denyResult := <-denyHandle.Wait()
	if err := runtime.ReleaseSession(ctx, denySessionID); err != nil {
		t.Fatalf("release deny-all session: %v", err)
	}
	if output := <-denyOutput; denyResult.Err != nil || denyResult.Code != 0 || !strings.Contains(string(output), `{"deny_all":true}`) {
		t.Fatalf("deny-all qualification output=%q exit=%+v", output, denyResult)
	}
}
