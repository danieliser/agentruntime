package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestDockerRuntime_Name(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{})
	if rt.Name() != "docker" {
		t.Fatalf("expected name 'docker', got %q", rt.Name())
	}
}

func TestDockerRecover_ReturnsSessionID(t *testing.T) {
	stateDir := t.TempDir()
	installFakeDocker(t, fmt.Sprintf(`#!/bin/sh
set -eu
state_dir=%q
if [ "$1" = "ps" ]; then
  if [ "$2" != "-aq" ]; then
    echo "expected stopped-container discovery, got: $*" >&2
    exit 5
  fi
	if [ "$3" != "--no-trunc" ]; then
		echo "expected canonical container IDs, got: $*" >&2
		exit 6
	fi
  printf '%%s\n' 'container-123'
  exit 0
fi
if [ "$1" = "inspect" ]; then
  if [ "$4" != "container-123" ]; then
    echo "unexpected container id: $4" >&2
    exit 3
  fi
  printf '%%s\n' '{"agentruntime.session_id":"sess-recovered","agentruntime.task_id":"task-recovered","agentruntime.generation":"2","agentruntime.idempotency_key":"job-recovered","agentruntime.request_hash":"sha256:request","agentruntime.agent":"claude","agentruntime.image_reference":"agentd:test","agentruntime.image_digest":"sha256:image","agentruntime.sandbox_profile":"docker-native-v1"}'
  exit 0
fi
if [ "$1" = "logs" ]; then
  if [ "$2" != "--follow" ] || [ "$3" != "--since=0" ] || [ "$4" != "container-123" ]; then
    echo "unexpected docker logs args: $*" >&2
    exit 4
  fi
  exit 0
fi
if [ "$1" = "attach" ]; then
  while IFS= read -r input; do :; done
  exit 0
fi
if [ "$1" = "wait" ]; then
  printf '0\n'
  exit 0
fi
if [ "$1" = "port" ]; then
  : >"$state_dir/port-called"
  exit 1
fi
echo "unexpected docker command: $1" >&2
exit 2
`, stateDir))

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	handles, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover failed: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 recovered handle, got %d", len(handles))
	}

	recovered, ok := handles[0].(*nativeDockerHandle)
	if !ok {
		t.Fatalf("expected nativeDockerHandle, got %T", handles[0])
	}
	if recovered.recovery.SessionID != "sess-recovered" {
		t.Fatalf("expected session ID from label, got %q", recovered.recovery.SessionID)
	}
	if recovered.recovery.TaskID != "task-recovered" {
		t.Fatalf("expected task ID from label, got %q", recovered.recovery.TaskID)
	}

	info := handles[0].RecoveryInfo()
	if info == nil {
		t.Fatal("expected recovery info")
	}
	if info.SessionID != "sess-recovered" {
		t.Fatalf("expected recovery info session ID %q, got %q", "sess-recovered", info.SessionID)
	}
	if info.Generation != 2 || info.IdempotencyKey != "job-recovered" || info.RequestHash != "sha256:request" || info.AgentName != "claude" {
		t.Fatalf("expected durable recovery labels, got %+v", info)
	}
	if info.ImageReference != "agentd:test" || info.ImageDigest != "sha256:image" || info.SandboxProfile != "docker-native-v1" {
		t.Fatalf("expected runtime identity labels, got %+v", info)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "port-called")); !os.IsNotExist(err) {
		t.Fatal("durable recovery queried the compatibility sidecar port")
	}
	_ = handles[0].Stdin().Close()
}

func TestDockerRunCarriesDurableGenerationLabels(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	args, err := rt.buildRunArgs(SpawnConfig{
		Cmd: []string{"echo"}, SessionID: "durable-session", TaskID: "task",
		Generation: 3, IdempotencyKey: "job-123", RequestHash: "sha256:abc",
		ImageReference: "ubuntu:22.04", ImageDigest: "sha256:image-id", SandboxProfile: "docker-native-v1",
	})
	if err != nil {
		t.Fatalf("build durable args: %v", err)
	}
	for _, label := range []string{
		"agentruntime.session_id=durable-session",
		"agentruntime.generation=3",
		"agentruntime.idempotency_key=job-123",
		"agentruntime.request_hash=sha256:abc",
		"agentruntime.image_reference=ubuntu:22.04",
		"agentruntime.image_digest=sha256:image-id",
		"agentruntime.sandbox_profile=docker-native-v1",
	} {
		if !hasFlagValue(args, "--label", label) {
			t.Errorf("missing label %q in %v", label, args)
		}
	}
}

func TestDockerDurableNativeRunHasNoSidecarTransport(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "agent:test"})
	args, err := rt.buildRunArgs(SpawnConfig{
		SessionID: "native-session", AgentName: "claude", Generation: 1,
		IdempotencyKey: "native-job", RequestHash: "sha256:native",
		Cmd: []string{"claude", "--output-format", "stream-json"},
	})
	if err != nil {
		t.Fatalf("build native Docker args: %v", err)
	}
	for _, forbidden := range []string{"--rm", "9090"} {
		if containsArg(args, forbidden) {
			t.Errorf("native Docker args contain sidecar flag %q: %v", forbidden, args)
		}
	}
	for flag, value := range map[string]string{
		"--log-driver": "json-file",
		"--entrypoint": "claude",
	} {
		if !hasFlagValue(args, flag, value) {
			t.Errorf("native Docker args missing %s %s: %v", flag, value, args)
		}
	}
	if !containsArg(args, "-i") {
		t.Errorf("native Docker args do not keep stdin open: %v", args)
	}
	imageIndex := indexArg(args, "agent:test")
	if imageIndex < 0 || imageIndex+2 >= len(args) || args[imageIndex+1] != "--output-format" || args[imageIndex+2] != "stream-json" {
		t.Fatalf("native command arguments after image = %v", args)
	}
}

func TestDockerClaudeUsesDirectNativeRunWithoutDurableGeneration(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "agent:test"})
	spec, err := rt.prepareRun(SpawnConfig{
		SessionID: "legacy-session", AgentName: "claude",
		Cmd: []string{"claude", "--output-format", "stream-json", "-p", "hello"},
	})
	if err != nil {
		t.Fatalf("build direct Docker args: %v", err)
	}
	defer spec.cleanup()
	args := spec.args
	for _, forbidden := range []string{"--rm", "9090"} {
		if containsArg(args, forbidden) {
			t.Errorf("direct Docker args contain sidecar flag %q: %v", forbidden, args)
		}
	}
	if hasFlagValue(args, "-p", "0:9090") {
		t.Errorf("direct Docker args publish the sidecar port: %v", args)
	}
	if !hasFlagValue(args, "--entrypoint", "claude") {
		t.Fatalf("direct Docker args do not launch Claude: %v", args)
	}
	imageIndex := indexArg(args, "agent:test")
	if imageIndex < 0 || imageIndex+4 >= len(args) || args[imageIndex+4] != "hello" {
		t.Fatalf("direct Claude arguments after image = %v", args)
	}
	envFile := flagValue(args, "--env-file")
	contents, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if strings.Contains(string(contents), "AGENT_CMD=") || strings.Contains(string(contents), "AGENT_PROMPT=") {
		t.Fatalf("direct native run retained sidecar environment: %q", contents)
	}
}

func TestDockerGenericCommandAlsoBypassesExecutionSidecar(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "agent:test"})
	spec, err := rt.prepareRun(SpawnConfig{
		SessionID: "generic-session", Cmd: []string{"echo", "hello"},
	})
	if err != nil {
		t.Fatalf("prepare direct generic run: %v", err)
	}
	defer spec.cleanup()
	if containsArg(spec.args, "--rm") || hasFlagValue(spec.args, "-p", "0:9090") {
		t.Fatalf("generic Docker run retained execution sidecar flags: %v", spec.args)
	}
	if !hasFlagValue(spec.args, "--entrypoint", "echo") {
		t.Fatalf("generic Docker run missing direct entrypoint: %v", spec.args)
	}
	imageIndex := indexArg(spec.args, "agent:test")
	if imageIndex < 0 || imageIndex+1 >= len(spec.args) || spec.args[imageIndex+1] != "hello" {
		t.Fatalf("generic command arguments after image = %v", spec.args)
	}
}

func TestNativeDockerHandleUsesAttachLogsAndWait(t *testing.T) {
	stateDir := t.TempDir()
	installFakeDocker(t, fmt.Sprintf(`#!/bin/sh
set -eu
state_dir=%q
case "$1" in
  attach)
    IFS= read -r input
    printf '%%s\n' "$input" >"$state_dir/stdin"
    ;;
  logs)
    [ "$2" = "--follow" ]
    [ "$3" = "--since=0" ]
    [ "$4" = "container-native" ]
    printf '{"type":"system","session_id":"provider-1"}\n'
    ;;
  wait)
    [ "$2" = "container-native" ]
    printf '7\n'
    ;;
  inspect)
    [ "$3" = "{{json .State}}" ]
    [ "$4" = "container-native" ]
    printf '%%s\n' '{"Status":"exited","Running":false,"OOMKilled":false,"Dead":false,"ExitCode":7,"Error":"","StartedAt":"2026-08-09T12:00:00Z","FinishedAt":"2026-08-09T12:00:01Z"}'
    ;;
  kill)
    [ "$2" = "container-native" ]
    ;;
  *)
    echo "unexpected docker command: $*" >&2
    exit 2
    ;;
esac
`, stateDir))

	handle, err := newNativeDockerHandle("", "container-native", RecoveryInfo{})
	if err != nil {
		t.Fatalf("new native Docker handle: %v", err)
	}
	if _, err := io.WriteString(handle.Stdin(), "hello native\n"); err != nil {
		t.Fatalf("write native stdin: %v", err)
	}
	if err := handle.Stdin().Close(); err != nil {
		t.Fatalf("close native stdin: %v", err)
	}

	stdout, err := io.ReadAll(handle.Stdout())
	if err != nil {
		t.Fatalf("read native stdout: %v", err)
	}
	if got, want := string(stdout), "{\"type\":\"system\",\"session_id\":\"provider-1\"}\n"; got != want {
		t.Fatalf("native stdout = %q, want %q", got, want)
	}
	result := <-handle.Wait()
	if result.Err != nil || result.Code != 7 {
		t.Fatalf("native wait = %+v, want code 7", result)
	}
	if err := waitForFile(filepath.Join(stateDir, "stdin"), 2*time.Second); err != nil {
		t.Fatalf("native attach did not receive stdin: %v", err)
	}
	input, err := os.ReadFile(filepath.Join(stateDir, "stdin"))
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	if string(input) != "hello native\n" {
		t.Fatalf("captured stdin = %q", input)
	}
	if handle.RuntimeID() != "container-native" || !handle.NativeStdio() {
		t.Fatalf("native handle identity = %q, native=%v", handle.RuntimeID(), handle.NativeStdio())
	}
}

func TestNativeDockerHandleInspectsOOMTerminalProof(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
case "$1" in
  attach)
    while IFS= read -r input; do :; done
    ;;
  logs)
    ;;
  wait)
    printf '137\n'
    ;;
  inspect)
    [ "$2" = "--format" ]
    [ "$3" = "{{json .State}}" ]
    [ "$4" = "container-oom" ]
    printf '%s\n' '{"Status":"exited","Running":false,"OOMKilled":true,"Dead":false,"ExitCode":137,"Error":"","StartedAt":"2026-08-09T12:00:00.123456789Z","FinishedAt":"2026-08-09T12:00:05.987654321Z"}'
    ;;
  kill)
    ;;
  *)
    echo "unexpected docker command: $*" >&2
    exit 2
    ;;
esac
`)
	handle, err := newNativeDockerHandle("", "container-oom", RecoveryInfo{})
	if err != nil {
		t.Fatalf("new native Docker handle: %v", err)
	}
	_ = handle.Stdin().Close()
	result := <-handle.Wait()
	if result.Err != nil || result.Code != 137 || !result.OOMKilled || result.Signal != "SIGKILL" {
		t.Fatalf("OOM terminal proof = %+v", result)
	}
	if result.StartedAt.Format(time.RFC3339Nano) != "2026-08-09T12:00:00.123456789Z" ||
		result.EndedAt.Format(time.RFC3339Nano) != "2026-08-09T12:00:05.987654321Z" {
		t.Fatalf("OOM timestamps = %s .. %s", result.StartedAt, result.EndedAt)
	}
}

func TestDockerSpawnDurableNativeSkipsSidecarBridge(t *testing.T) {
	stateDir := t.TempDir()
	installFakeDocker(t, fmt.Sprintf(`#!/bin/sh
set -eu
state_dir=%q
printf '%%s\n' "$*" >>"$state_dir/commands"
case "$1" in
  image)
    printf 'sha256:native-image\n'
    ;;
  network)
    [ "$2" = "inspect" ]
    ;;
  inspect)
    if [ "$3" = "{{json .State}}" ]; then
      printf '%%s\n' '{"Status":"exited","Running":false,"OOMKilled":false,"Dead":false,"ExitCode":0,"Error":"","StartedAt":"2026-08-09T12:00:00Z","FinishedAt":"2026-08-09T12:00:01Z"}'
    elif [ "$3" = "{{.Image}}" ]; then
      printf 'sha256:native-image\n'
    else
      printf 'true\n'
    fi
    ;;
  run)
    printf 'container-native\n'
    ;;
  attach)
    while IFS= read -r input; do :; done
    ;;
  logs)
    printf '{"type":"system","session_id":"provider-1"}\n'
    ;;
  wait)
    printf '0\n'
    ;;
  kill)
    ;;
  *)
    echo "unexpected docker command: $*" >&2
    exit 2
    ;;
esac
`, stateDir))

	runtime := NewDockerRuntime(DockerConfig{Image: "agent:test"})
	handle, err := runtime.Spawn(context.Background(), SpawnConfig{
		SessionID: "native-session", AgentName: "claude", Generation: 1,
		IdempotencyKey: "native-job", RequestHash: "sha256:native",
		Cmd: []string{"claude", "--output-format", "stream-json"},
	})
	if err != nil {
		t.Fatalf("spawn durable native Docker session: %v", err)
	}
	if _, ok := handle.(*nativeDockerHandle); !ok {
		t.Fatalf("durable native spawn returned %T", handle)
	}
	identified, ok := handle.(RuntimeImageIdentifiedHandle)
	if !ok || identified.RuntimeImageDigest() != "sha256:native-image" {
		t.Fatalf("durable image identity = %T %q", handle, identified.RuntimeImageDigest())
	}
	_ = handle.Stdin().Close()
	result := <-handle.Wait()
	if result.Err != nil || result.Code != 0 {
		t.Fatalf("native wait = %+v", result)
	}

	commands, err := os.ReadFile(filepath.Join(stateDir, "commands"))
	if err != nil {
		t.Fatalf("read Docker commands: %v", err)
	}
	if strings.Contains(string(commands), "port container-native") {
		t.Fatalf("durable native spawn queried sidecar port:\n%s", commands)
	}
}

func indexArg(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
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
if [ "$1" = "attach" ]; then
  while IFS= read -r input; do :; done
  exit 0
fi
if [ "$1" = "wait" ]; then
  printf '0\n'
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

func TestDockerSpawn_DirectProviderEnv(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"echo", "hello world"},
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

	if strings.Contains(string(data), "AGENT_CMD=") || strings.Contains(string(data), "AGENT_PROMPT=") {
		t.Fatalf("direct provider env contains sidecar configuration: %q", data)
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
	if strings.Contains(contents, "AGENT_CMD=") {
		t.Fatalf("direct command leaked obsolete AGENT_CMD into env file: %q", contents)
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
	if !containsArg(spec.args, "custom:latest") || containsArg(spec.args, "ubuntu:22.04") {
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

func TestDockerVolumeName(t *testing.T) {
	tests := []struct {
		sessionID string
		expected  string
	}{
		{"abc123", "agentruntime-vol-abc123"},
		{"very-long-session-id-with-many-chars", "agentruntime-vol-very-long-session-id-with-many-chars"},
	}
	for _, tc := range tests {
		got := dockerVolumeName(tc.sessionID)
		if got != tc.expected {
			t.Errorf("dockerVolumeName(%q) = %q, want %q", tc.sessionID, got, tc.expected)
		}
	}
}

func TestDockerPrepareRun_PersistSession_CreatesVolumeMount(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1" = "volume" ] && [ "$2" = "create" ]; then
  # Capture the volume name (last arg)
  shift 4  # skip "volume", "create", "--label", label
  volume_name="$1"
  exit 0
fi
# Handle init volume permissions run
if [ "$1" = "run" ] && [ "$2" = "--rm" ]; then
  exit 0
fi
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	workDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"claude"},
		SessionID: "persist-1234",
		Request: &apischema.SessionRequest{
			WorkDir:        workDir,
			PersistSession: true,
			Claude:         &apischema.ClaudeConfig{},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	// Check that volume mount was added to args
	expectedMount := "agentruntime-vol-persist-1234:/home/agent/.claude/projects:rw"
	if !hasFlagValue(spec.args, "-v", expectedMount) {
		t.Fatalf("expected volume mount %q in args, got %v", expectedMount, spec.args)
	}
}

func TestDockerPrepareRun_NoPersistSession_NoVolume(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	workDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"claude"},
		SessionID: "nopersist-1234",
		Request: &apischema.SessionRequest{
			WorkDir:        workDir,
			PersistSession: false,
			Claude:         &apischema.ClaudeConfig{},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	// Check that NO volume mount was added
	for i := 0; i < len(spec.args)-1; i++ {
		if spec.args[i] == "-v" && strings.Contains(spec.args[i+1], ".claude/projects") {
			t.Fatalf("unexpected volume mount in args: %v", spec.args)
		}
	}
}

func TestDockerPrepareRun_VolumeMount_SkipsValidation(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
# Handle init volume permissions run
if [ "$1" = "run" ] && [ "$2" = "--rm" ]; then
  exit 0
fi
exit 2
`)
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"echo"},
		SessionID: "vol-skip-validate",
		Request: &apischema.SessionRequest{
			Mounts: []apischema.Mount{{
				Host:      "my-volume",
				Container: "/data",
				Mode:      "rw",
				Type:      "volume",
			}},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun with volume mount should not fail validation: %v", err)
	}
	defer spec.cleanup()

	// Check that volume mount is in args
	if !hasFlagValue(spec.args, "-v", "my-volume:/data:rw") {
		t.Fatalf("expected volume mount in args, got %v", spec.args)
	}
}

func TestDockerPrepareRun_ReuseVolume(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1" = "volume" ] && [ "$2" = "create" ]; then
  exit 1  # Fail to create (should not be called when reusing)
fi
# Handle init volume permissions run
if [ "$1" = "run" ] && [ "$2" = "--rm" ]; then
  exit 0
fi
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	workDir := t.TempDir()

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:        []string{"claude"},
		SessionID:  "new-session-5678",
		VolumeName: "agentruntime-vol-old-session-1234", // Reuse existing volume
		Request: &apischema.SessionRequest{
			WorkDir:        workDir,
			PersistSession: true,
			Claude:         &apischema.ClaudeConfig{},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun with reused volume failed: %v", err)
	}
	defer spec.cleanup()

	// Check that the reused volume mount is in args
	expectedMount := "agentruntime-vol-old-session-1234:/home/agent/.claude/projects:rw"
	if !hasFlagValue(spec.args, "-v", expectedMount) {
		t.Fatalf("expected reused volume mount %q in args, got %v", expectedMount, spec.args)
	}
}

// TestDockerPrepareRun_ChatMountsWithPersist exercises the full chat path:
// user-supplied volume mounts + PersistSession + materializer. All three
// should produce -v flags in the final docker run args.
func TestDockerPrepareRun_ChatMountsWithPersist(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1" = "volume" ] && [ "$2" = "create" ]; then
  echo "agentruntime-vol-chat-sess"
  exit 0
fi
# Handle init volume permissions run
if [ "$1" = "run" ] && [ "$2" = "--rm" ]; then
  exit 0
fi
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "agentruntime-agent:latest"})

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:        []string{"claude"},
		SessionID:  "chat-sess-1234",
		VolumeName: "agentruntime-chat-mybot",
		Request: &apischema.SessionRequest{
			Agent:          "claude",
			Interactive:    true,
			PersistSession: true,
			Claude:         &apischema.ClaudeConfig{},
			// Simulates what the chat manager sends: user workspace volume + chat volume
			Mounts: []apischema.Mount{
				{Host: "persist-workspace-test", Container: "/workspace/persist", Mode: "rw", Type: "volume"},
				{Host: "agentruntime-chat-mybot", Container: "/home/agent/.claude/projects", Mode: "rw", Type: "volume"},
			},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	// User workspace volume must appear as a -v flag.
	userMount := "persist-workspace-test:/workspace/persist:rw"
	if !hasFlagValue(spec.args, "-v", userMount) {
		t.Fatalf("expected user volume mount %q in docker args:\n%v", userMount, spec.args)
	}

	// Chat volume must appear as a -v flag.
	chatMount := "agentruntime-chat-mybot:/home/agent/.claude/projects:rw"
	if !hasFlagValue(spec.args, "-v", chatMount) {
		t.Fatalf("expected chat volume mount %q in docker args:\n%v", chatMount, spec.args)
	}

	// Materializer's session dir mount must also be present.
	foundClaudeDir := false
	for i, arg := range spec.args {
		if arg == "-v" && i+1 < len(spec.args) && strings.Contains(spec.args[i+1], ":/home/agent/.claude:rw") {
			foundClaudeDir = true
			break
		}
	}
	if !foundClaudeDir {
		t.Fatalf("expected materializer claude dir mount in docker args:\n%v", spec.args)
	}
}

// TestInitVolumePermissions_RunsChown verifies that initVolumePermissions
// runs a docker container as root to chown volume mount points.
func TestInitVolumePermissions_RunsChown(t *testing.T) {
	var capturedArgs []string
	installFakeDocker(t, `#!/bin/sh
set -eu
# Capture all args for inspection
echo "$@" > /tmp/init-vol-args-test
exit 0
`)

	rt := NewDockerRuntime(DockerConfig{Image: "agentruntime-agent:latest"})
	mounts := []apischema.Mount{
		{Host: "vol-a", Container: "/workspace/persist", Mode: "rw", Type: "volume"},
		{Host: "vol-b", Container: "/data", Mode: "rw", Type: "volume"},
		{Host: "/real/path", Container: "/code", Mode: "rw", Type: "bind"},
	}
	_ = capturedArgs

	err := rt.initVolumePermissions(context.Background(), "agentruntime-agent:latest", mounts)
	if err != nil {
		t.Fatalf("initVolumePermissions failed: %v", err)
	}
}

// TestInitVolumePermissions_SkipsWhenNoVolumes verifies that no docker command
// is run when there are no volume-type mounts.
func TestInitVolumePermissions_SkipsWhenNoVolumes(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
exit 99
`)

	rt := NewDockerRuntime(DockerConfig{Image: "test:latest"})
	mounts := []apischema.Mount{
		{Host: "/tmp/work", Container: "/workspace", Mode: "rw", Type: "bind"},
	}

	err := rt.initVolumePermissions(context.Background(), "test:latest", mounts)
	if err != nil {
		t.Fatalf("should not fail for bind-only mounts: %v", err)
	}
}

// TestDockerPrepareRun_NoDuplicateVolumeMount verifies that when the request
// already has a volume mount at /home/agent/.claude/projects (e.g., from the
// chat manager), PersistSession does NOT add a second one that would shadow it.
func TestDockerPrepareRun_NoDuplicateVolumeMount(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "--rm" ]; then
  exit 0
fi
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "agentruntime-agent:latest"})
	workDir := t.TempDir()

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"claude"},
		SessionID: "no-dup-vol",
		Request: &apischema.SessionRequest{
			WorkDir:        workDir,
			PersistSession: true,
			Claude:         &apischema.ClaudeConfig{},
			Mounts: []apischema.Mount{
				{Host: "agentruntime-chat-mybot", Container: "/home/agent/.claude/projects", Mode: "rw", Type: "volume"},
			},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	// Count how many -v flags target /home/agent/.claude/projects
	count := 0
	for i, arg := range spec.args {
		if arg == "-v" && i+1 < len(spec.args) && strings.Contains(spec.args[i+1], "/home/agent/.claude/projects") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 volume mount at /home/agent/.claude/projects, got %d", count)
	}

	// The surviving mount should be the chat volume, not a per-session one
	if !hasFlagValue(spec.args, "-v", "agentruntime-chat-mybot:/home/agent/.claude/projects:rw") {
		t.Fatalf("expected chat volume mount to survive, got: %v", spec.args)
	}
}
