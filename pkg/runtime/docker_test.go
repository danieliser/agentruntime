package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/gorilla/websocket"
)

func TestDockerRuntime_Name(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{})
	if rt.Name() != "docker" {
		t.Fatalf("expected name 'docker', got %q", rt.Name())
	}
}

func TestDockerRecover_ReturnsSessionID(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1" = "ps" ]; then
  printf '%s\n' 'container-123'
  exit 0
fi
if [ "$1" = "inspect" ]; then
  if [ "$4" != "container-123" ]; then
    echo "unexpected container id: $4" >&2
    exit 3
  fi
  printf '%s\n' '{"agentruntime.session_id":"sess-recovered","agentruntime.task_id":"task-recovered"}'
  exit 0
fi
if [ "$1" = "logs" ]; then
  if [ "$2" != "--follow" ] || [ "$3" != "--since=0" ] || [ "$4" != "container-123" ]; then
    echo "unexpected docker logs args: $*" >&2
    exit 4
  fi
  exit 0
fi
echo "unexpected docker command: $1" >&2
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	handles, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover failed: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 recovered handle, got %d", len(handles))
	}

	recovered, ok := handles[0].(*recoveredDockerHandle)
	if !ok {
		t.Fatalf("expected recoveredDockerHandle, got %T", handles[0])
	}
	if recovered.SessionID != "sess-recovered" {
		t.Fatalf("expected session ID from label, got %q", recovered.SessionID)
	}
	if recovered.TaskID != "task-recovered" {
		t.Fatalf("expected task ID from label, got %q", recovered.TaskID)
	}

	info := handles[0].RecoveryInfo()
	if info == nil {
		t.Fatal("expected recovery info")
	}
	if info.SessionID != "sess-recovered" {
		t.Fatalf("expected recovery info session ID %q, got %q", "sess-recovered", info.SessionID)
	}
}

func TestRecoveredDockerHandle_StdoutFromLogs(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1" = "ps" ]; then
  printf '%s\n' 'container-123'
  exit 0
fi
if [ "$1" = "inspect" ]; then
  printf '%s\n' '{"agentruntime.session_id":"sess-recovered","agentruntime.task_id":"task-recovered"}'
  exit 0
fi
if [ "$1" = "logs" ]; then
  if [ "$2" != "--follow" ] || [ "$3" != "--since=0" ] || [ "$4" != "container-123" ]; then
    echo "unexpected docker logs args: $*" >&2
    exit 4
  fi
  printf 'recovered stdout line\n'
  exit 0
fi
echo "unexpected docker command: $1" >&2
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	handles, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover failed: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 recovered handle, got %d", len(handles))
	}

	got, err := io.ReadAll(handles[0].Stdout())
	if err != nil {
		t.Fatalf("read recovered stdout: %v", err)
	}
	if string(got) != "recovered stdout line\n" {
		t.Fatalf("expected recovered stdout from docker logs, got %q", string(got))
	}

	result := <-handles[0].Wait()
	if result.Err != nil {
		t.Fatalf("wait returned error: %v", result.Err)
	}
	if result.Code != 0 {
		t.Fatalf("expected zero exit code from docker logs follower, got %d", result.Code)
	}
}

func TestDockerRecover_PrefersSidecarWhenAvailable(t *testing.T) {
	sidecarPort := startFakeDockerSidecar(t)
	installFakeDocker(t, fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  ps)
    printf '%%s\n' 'container-123'
    ;;
  inspect)
    printf '%%s\n' '{"agentruntime.session_id":"sess-sidecar","agentruntime.task_id":"task-sidecar"}'
    ;;
  port)
    printf '0.0.0.0:%s\n'
    ;;
  stop|rm)
    exit 0
    ;;
  *)
    echo "unexpected docker command: $1" >&2
    exit 2
    ;;
esac
`, sidecarPort))

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	handles, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover failed: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 recovered handle, got %d", len(handles))
	}

	wsRecovered, ok := handles[0].(*wsHandle)
	if !ok {
		t.Fatalf("expected wsHandle, got %T", handles[0])
	}
	t.Cleanup(func() {
		_ = wsRecovered.Kill()
	})

	info := wsRecovered.RecoveryInfo()
	if info == nil {
		t.Fatal("expected recovery info")
	}
	if info.SessionID != "sess-sidecar" || info.TaskID != "task-sidecar" {
		t.Fatalf("unexpected recovery info: %+v", info)
	}
}

func TestDockerSpawn_SecurityFlagsPresent(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"echo", "ok"},
		SessionID: "1234567890abcdef",
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !containsArg(spec.args, "--init") {
		t.Fatalf("expected --init in args, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "--cap-drop", "ALL") {
		t.Fatalf("expected --cap-drop ALL, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "--cap-add", "DAC_OVERRIDE") {
		t.Fatalf("expected --cap-add DAC_OVERRIDE, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "--security-opt", "no-new-privileges:true") {
		t.Fatalf("expected no-new-privileges security opt, got %v", spec.args)
	}
}

func TestDockerSpawn_WSBased_DetachedMode(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"echo", "ok"},
		SessionID: "detached-mode-1234",
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !containsArg(spec.args, "-d") {
		t.Fatalf("expected -d in args, got %v", spec.args)
	}
	if containsArg(spec.args, "-i") {
		t.Fatalf("did not expect -i in args, got %v", spec.args)
	}
}

func TestDockerSpawn_WSBased_PortMapping(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"echo", "ok"},
		SessionID: "port-mapping-1234",
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !hasFlagValue(spec.args, "-p", "0:9090") {
		t.Fatalf("expected -p 0:9090 in args, got %v", spec.args)
	}
}

func TestDockerSpawn_WSBased_AgentCmdEnv(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	cmd := []string{"echo", "hello world"}

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       cmd,
		SessionID: "agent-cmd-env-1234",
		Request: &apischema.SessionRequest{
			Env: map[string]string{
				"VISIBLE_VAR": "docker-value",
			},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	envFile := flagValue(spec.args, "--env-file")
	if envFile == "" {
		t.Fatalf("expected --env-file in args, got %v", spec.args)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}

	encodedCmd, err := json.Marshal([]string{cmd[0]})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	if !strings.Contains(string(data), "AGENT_CMD="+string(encodedCmd)+"\n") {
		t.Fatalf("expected AGENT_CMD in env file, got %q", string(data))
	}
	if !strings.Contains(string(data), "HTTP_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected HTTP_PROXY in env file, got %q", string(data))
	}
	if !strings.Contains(string(data), "HTTPS_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected HTTPS_PROXY in env file, got %q", string(data))
	}
	if !strings.Contains(string(data), "NO_PROXY=localhost,127.0.0.1,host.docker.internal,host-gateway\n") {
		t.Fatalf("expected NO_PROXY in env file, got %q", string(data))
	}
}

func TestDockerSpawn_V2_AgentCmdIsBinaryOnly(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"claude", "--dangerously-skip-permissions", "-p", "fix the bug", "--output-format", "stream-json"},
		Prompt:    "fix the bug",
		SessionID: "v2-agent-cmd-binary-only",
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	envFile := flagValue(spec.args, "--env-file")
	if envFile == "" {
		t.Fatalf("expected --env-file in args, got %v", spec.args)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(data), "AGENT_CMD=[\"claude\"]\n") {
		t.Fatalf("expected binary-only AGENT_CMD, got %q", string(data))
	}
	if strings.Contains(string(data), "fix the bug") {
		t.Fatalf("did not expect prompt embedded in AGENT_CMD env, got %q", string(data))
	}
}

func TestDockerSpawn_V2_PromptSentViaWS(t *testing.T) {
	received := make(chan wsClientFrame, 1)
	sidecarPort := startFakeDockerV2Sidecar(t, received)

	installFakeDocker(t, fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  network)
    case "$2" in
      inspect)
        echo "Error: No such network: agentruntime-agents" >&2
        exit 1
        ;;
      create)
        exit 0
        ;;
    esac
    ;;
  inspect)
    if [ "$2" = "--format" ]; then
      echo "Error: No such object: agentruntime-proxy" >&2
      exit 1
    fi
    ;;
  run)
    shift
    name=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --name)
          name=$2
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    if [ "$name" = "agentruntime-proxy" ]; then
      printf '%%s\n' 'proxy-container'
      exit 0
    fi
    printf '%%s\n' 'container-v2-prompt'
    ;;
  port)
    printf '0.0.0.0:%s\n'
    ;;
  stop|rm)
    exit 0
    ;;
  *)
    echo "unexpected docker command: $1" >&2
    exit 2
    ;;
esac
`, sidecarPort))

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	handle, err := rt.Spawn(testContext(t), SpawnConfig{
		Cmd:       []string{"claude"},
		Prompt:    "fix the auth bug",
		SessionID: "v2-prompt-over-ws",
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	t.Cleanup(func() {
		_ = handle.Kill()
	})

	select {
	case frame := <-received:
		if frame.Type != "prompt" {
			t.Fatalf("expected prompt frame type, got %q", frame.Type)
		}
		data, ok := frame.Data.(map[string]any)
		if !ok {
			t.Fatalf("expected prompt data object, got %T", frame.Data)
		}
		if got := data["content"]; got != "fix the auth bug" {
			t.Fatalf("expected prompt content %q, got %v", "fix the auth bug", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for prompt frame")
	}
}

func TestDockerSpawn_V2_LocalUnchanged(t *testing.T) {
	rt := NewLocalRuntime()
	handle, err := rt.Spawn(testContext(t), SpawnConfig{
		Cmd:    []string{"/bin/echo", "prompt from cmd"},
		Prompt: "prompt from field",
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	got, err := io.ReadAll(handle.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if string(got) != "prompt from cmd\n" {
		t.Fatalf("expected local runtime to execute full Cmd unchanged, got %q", string(got))
	}
}

func TestDockerSpawn_WSBased_NoCommandAfterImage(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	args, err := rt.buildRunArgs(SpawnConfig{
		Cmd:       []string{"echo", "ok"},
		SessionID: "no-command-after-image-1234",
	})
	if err != nil {
		t.Fatalf("buildRunArgs failed: %v", err)
	}

	if got := args[len(args)-1]; got != "ubuntu:22.04" {
		t.Fatalf("expected image to be final arg, got %q in args %v", got, args)
	}
}

func TestDockerSpawn_MountsFromRequest(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	workDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"pwd"},
		SessionID: "mount-check-1234",
		Request: &apischema.SessionRequest{
			WorkDir: workDir,
			Mounts: []apischema.Mount{{
				Host:      dataDir,
				Container: "/data",
				Mode:      "ro",
			}},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !hasFlagValue(spec.args, "-v", workDir+":/workspace:rw") {
		t.Fatalf("expected workdir mount in args, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "-v", dataDir+":/data:ro") {
		t.Fatalf("expected explicit request mount in args, got %v", spec.args)
	}
}

func TestDockerSpawn_EnvFileCreatedAndDeleted(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"env"},
		SessionID: "env-file-1234",
		Request: &apischema.SessionRequest{
			Env: map[string]string{
				"VISIBLE_VAR": "docker-value",
			},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}

	envFile := flagValue(spec.args, "--env-file")
	if envFile == "" {
		t.Fatalf("expected --env-file in args, got %v", spec.args)
	}

	info, err := os.Stat(envFile)
	if err != nil {
		t.Fatalf("expected env file to exist before cleanup: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected env file perms 0600, got %o", info.Mode().Perm())
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	contents := string(data)
	if !strings.Contains(contents, "AGENT_CMD=[\"env\"]\n") {
		t.Fatalf("expected AGENT_CMD in env file, got %q", contents)
	}
	if !strings.Contains(contents, "VISIBLE_VAR=docker-value\n") {
		t.Fatalf("expected VISIBLE_VAR in env file, got %q", contents)
	}
	if !strings.Contains(contents, "HTTP_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected HTTP_PROXY in env file, got %q", contents)
	}
	if !strings.Contains(contents, "HTTPS_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected HTTPS_PROXY in env file, got %q", contents)
	}
	if !strings.Contains(contents, "NO_PROXY=localhost,127.0.0.1,host.docker.internal,host-gateway\n") {
		t.Fatalf("unexpected env file contents %q", string(data))
	}

	spec.cleanup()

	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("expected env file deleted after cleanup, got err=%v", err)
	}
}

func TestDockerSpawn_ContainerNaming(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"echo", "ok"},
		SessionID: "abcdef1234567890",
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !hasFlagValue(spec.args, "--name", "agentruntime-abcdef12") {
		t.Fatalf("expected truncated container name, got %v", spec.args)
	}
}

func TestDockerSpawn_ResourceLimits(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{
		Image:   "ubuntu:22.04",
		Network: "bridge",
	})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"echo", "ok"},
		SessionID: "resource-1234",
		Request: &apischema.SessionRequest{
			Container: &apischema.ContainerConfig{
				Image:   "custom:latest",
				Memory:  "4g",
				CPUs:    2.5,
				Network: "none",
			},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !hasFlagValue(spec.args, "--memory", "4g") {
		t.Fatalf("expected memory limit in args, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "--cpus", "2.5") {
		t.Fatalf("expected cpu limit in args, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "--network", "bridge") {
		t.Fatalf("expected configured network in args, got %v", spec.args)
	}
	if spec.args[len(spec.args)-1] != "custom:latest" {
		t.Fatalf("expected resource image override in args, got %v", spec.args)
	}
}

func flagValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func startFakeDockerV2Sidecar(t *testing.T, received chan<- wsClientFrame) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case dockerSidecarHealthPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":     "ok",
				"agent_type": "claude",
			})
		case "/ws":
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade websocket: %v", err)
			}
			defer conn.Close()

			if err := conn.WriteJSON(wsServerFrame{Type: "connected"}); err != nil {
				t.Fatalf("write connected: %v", err)
			}

			var frame wsClientFrame
			if err := conn.ReadJSON(&frame); err != nil {
				t.Fatalf("read prompt frame: %v", err)
			}
			received <- frame

			code := 0
			if err := conn.WriteJSON(wsServerFrame{Type: "exit", ExitCode: &code}); err != nil {
				t.Fatalf("write exit: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return u.Port()
}
