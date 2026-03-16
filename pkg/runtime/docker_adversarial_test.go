package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestDockerAdversarial_MountHostWithSpacesStaysSingleVArg(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	hostDir := filepath.Join(t.TempDir(), "dir with spaces")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("mkdir host dir: %v", err)
	}

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"pwd"},
		SessionID: "space-mount-1234",
		Request: &apischema.SessionRequest{
			Mounts: []apischema.Mount{{
				Host:      hostDir,
				Container: "/data",
				Mode:      "ro",
			}},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	want := hostDir + ":/data:ro"
	if !hasFlagValue(spec.args, "-v", want) {
		t.Fatalf("expected single -v arg %q, got %v", want, spec.args)
	}
}

func TestDockerAdversarial_MountContainerPathsStayLiteral(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	absoluteHost := filepath.Join(t.TempDir(), "absolute")
	relativeHost := filepath.Join(t.TempDir(), "relative")
	for _, dir := range []string{absoluteHost, relativeHost} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"pwd"},
		SessionID: "container-paths-1234",
		Request: &apischema.SessionRequest{
			Mounts: []apischema.Mount{
				{Host: absoluteHost, Container: "/absolute/path", Mode: "rw"},
				{Host: relativeHost, Container: "relative/path", Mode: "ro"},
			},
		},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !hasFlagValue(spec.args, "-v", absoluteHost+":/absolute/path:rw") {
		t.Fatalf("expected absolute container path mount, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "-v", relativeHost+":relative/path:ro") {
		t.Fatalf("expected relative container path mount to stay literal, got %v", spec.args)
	}
}

func TestDockerAdversarial_ImageMetacharactersPassedLiterally(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	image := "registry.example.com/agent:latest$(touch_pwned);echo"
	cmd := []string{"echo", "ok"}

	args, err := rt.buildRunArgs(SpawnConfig{
		Cmd:       cmd,
		SessionID: "image-meta-1234",
		Request: &apischema.SessionRequest{
			Container: &apischema.ContainerConfig{Image: image},
		},
	})
	if err != nil {
		t.Fatalf("buildRunArgs failed: %v", err)
	}

	got := args[len(args)-len(cmd)-1]
	if got != image {
		t.Fatalf("expected image %q, got %q in args %v", image, got, args)
	}
}

func TestDockerAdversarial_InvalidMemoryFailsGracefully(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1" != "run" ]; then
  echo "unexpected docker command: $1" >&2
  exit 2
fi
shift
while [ "$#" -gt 0 ]; do
  case "$1" in
    --memory)
      if [ "$2" = "lots" ]; then
        echo "docker: invalid memory value: lots" >&2
        exit 125
      fi
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
exit 0
`)

	rt := NewDockerRuntime(DockerConfig{Image: "alpine:latest"})
	handle, err := rt.Spawn(testContext(t), SpawnConfig{
		Cmd:       []string{"echo", "ignored"},
		SessionID: "invalid-memory-1234",
		Request: &apischema.SessionRequest{
			Container: &apischema.ContainerConfig{Memory: "lots"},
		},
	})
	if err != nil {
		t.Fatalf("spawn failed before docker returned an exit code: %v", err)
	}

	stdout, stderr, result := readProcessOutput(t, handle)
	if result.Err != nil {
		t.Fatalf("wait failed: %v (stderr=%q)", result.Err, stderr)
	}
	if result.Code == 0 {
		t.Fatalf("expected non-zero exit for invalid memory, got stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "invalid memory value") {
		t.Fatalf("expected docker stderr to mention invalid memory, got %q", stderr)
	}
}

func TestDockerAdversarial_EnvFileFormatsAndRejectsEdgeValues(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	t.Run("equals_and_flag_like_key_are_preserved", func(t *testing.T) {
		spec, err := rt.prepareRun(SpawnConfig{
			Cmd:       []string{"env"},
			SessionID: "env-equals-1234",
			Request: &apischema.SessionRequest{
				Env: map[string]string{
					"--rm":        "still-an-env",
					"WITH_EQUALS": "left=right=tail",
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
		contents := string(data)
		if !strings.Contains(contents, "--rm=still-an-env\n") {
			t.Fatalf("expected flag-like key to remain an env var, got %q", contents)
		}
		if !strings.Contains(contents, "WITH_EQUALS=left=right=tail\n") {
			t.Fatalf("expected equals signs to be preserved, got %q", contents)
		}
	})

	t.Run("newline_value_is_rejected", func(t *testing.T) {
		_, err := rt.prepareRun(SpawnConfig{
			Cmd:       []string{"env"},
			SessionID: "env-newline-1234",
			Request: &apischema.SessionRequest{
				Env: map[string]string{"BAD_VALUE": "line1\nline2"},
			},
		})
		if err == nil {
			t.Fatal("expected newline env value to be rejected")
		}
		if !strings.Contains(err.Error(), "invalid env value") {
			t.Fatalf("expected invalid env value error, got %v", err)
		}
	})

	t.Run("nul_value_is_rejected", func(t *testing.T) {
		_, err := rt.prepareRun(SpawnConfig{
			Cmd:       []string{"env"},
			SessionID: "env-nul-1234",
			Request: &apischema.SessionRequest{
				Env: map[string]string{"BAD_VALUE": "nul\x00byte"},
			},
		})
		if err == nil {
			t.Fatal("expected NUL env value to be rejected")
		}
		if !strings.Contains(err.Error(), "invalid env value") {
			t.Fatalf("expected invalid env value error, got %v", err)
		}
	})
}

func TestDockerAdversarial_FiftyMountsAllAppearInArgs(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	mounts := make([]apischema.Mount, 0, 50)
	for i := 0; i < 50; i++ {
		host := filepath.Join(t.TempDir(), fmt.Sprintf("mount-%02d", i))
		if err := os.MkdirAll(host, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", host, err)
		}
		mounts = append(mounts, apischema.Mount{
			Host:      host,
			Container: fmt.Sprintf("/mnt/%02d", i),
			Mode:      "ro",
		})
	}

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"true"},
		SessionID: "many-mounts-1234",
		Request:   &apischema.SessionRequest{Mounts: mounts},
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	values := dockerFlagValues(spec.args, "-v")
	if len(values) != len(mounts) {
		t.Fatalf("expected %d mount args, got %d in %v", len(mounts), len(values), spec.args)
	}
	for _, mount := range mounts {
		want := formatDockerMount(mount)
		if !containsString(values, want) {
			t.Fatalf("expected mount %q in args %v", want, values)
		}
	}
}

func TestDockerAdversarial_ContainerNameCollisionFailsOrUsesUniqueName(t *testing.T) {
	stateDir := t.TempDir()
	installFakeDocker(t, fmt.Sprintf(`#!/bin/sh
set -eu
state_dir=%q
if [ "$1" != "run" ]; then
  echo "unexpected docker command: $1" >&2
  exit 2
fi
name=""
hold=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --name)
      name=$2
      shift 2
      ;;
    __hold__)
      hold=1
      shift
      ;;
    *)
      shift
      ;;
  esac
done
if [ -z "$name" ]; then
  echo "missing container name" >&2
  exit 2
fi
marker="$state_dir/$name"
if [ -e "$marker" ]; then
  echo "docker: Error response from daemon: Conflict. The container name \"/$name\" is already in use." >&2
  exit 125
fi
: >"$marker"
if [ "$hold" -eq 1 ]; then
  while :; do sleep 1; done
fi
rm -f "$marker"
exit 0
`, stateDir))

	rt := NewDockerRuntime(DockerConfig{Image: "alpine:latest"})
	firstCfg := SpawnConfig{
		Cmd:       []string{"__hold__"},
		SessionID: "deadbeef-first",
	}
	secondCfg := SpawnConfig{
		Cmd:       []string{"echo", "ok"},
		SessionID: "deadbeef-second",
	}

	firstArgs, err := rt.buildRunArgs(firstCfg)
	if err != nil {
		t.Fatalf("buildRunArgs(first) failed: %v", err)
	}
	secondArgs, err := rt.buildRunArgs(secondCfg)
	if err != nil {
		t.Fatalf("buildRunArgs(second) failed: %v", err)
	}
	firstName := flagValue(firstArgs, "--name")
	secondName := flagValue(secondArgs, "--name")

	first, err := rt.Spawn(testContext(t), firstCfg)
	if err != nil {
		t.Fatalf("spawn(first) failed: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Kill()
	})

	if err := waitForFile(filepath.Join(stateDir, firstName), 2*time.Second); err != nil {
		t.Fatalf("first container marker not created: %v", err)
	}

	second, err := rt.Spawn(testContext(t), secondCfg)
	if err != nil {
		if firstName != secondName {
			t.Fatalf("expected unique names to avoid immediate spawn failure, got %v", err)
		}
		if !strings.Contains(err.Error(), "docker run start") {
			t.Fatalf("expected docker start error for collision, got %v", err)
		}
		return
	}

	stdout, stderr, result := readProcessOutput(t, second)
	if result.Err != nil {
		t.Fatalf("wait(second) failed: %v (stderr=%q)", result.Err, stderr)
	}

	if firstName == secondName {
		if result.Code == 0 {
			t.Fatalf("expected collision to fail for duplicate container name %q, got stdout=%q stderr=%q", firstName, stdout, stderr)
		}
		if !strings.Contains(stderr, "already in use") {
			t.Fatalf("expected duplicate-name error, got %q", stderr)
		}
		return
	}

	if result.Code != 0 {
		t.Fatalf("expected unique name path to succeed, got code=%d stderr=%q", result.Code, stderr)
	}
}

func TestDockerAdversarial_PTYAddsTFlag(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})

	args, err := rt.buildRunArgs(SpawnConfig{
		Cmd:       []string{"sh"},
		SessionID: "pty-flag-1234",
		PTY:       true,
	})
	if err != nil {
		t.Fatalf("buildRunArgs failed: %v", err)
	}

	if !containsArg(args, "-t") {
		t.Fatalf("expected -t in args when PTY is requested, got %v", args)
	}
}

func TestDockerAdversarial_RequestImageWinsOverRuntimeConfig(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	cmd := []string{"echo", "ok"}

	args, err := rt.buildRunArgs(SpawnConfig{
		Cmd:       cmd,
		SessionID: "request-image-1234",
		Request: &apischema.SessionRequest{
			Container: &apischema.ContainerConfig{Image: "busybox:1.36"},
		},
	})
	if err != nil {
		t.Fatalf("buildRunArgs failed: %v", err)
	}

	got := args[len(args)-len(cmd)-1]
	if got != "busybox:1.36" {
		t.Fatalf("expected request image to win, got %q in args %v", got, args)
	}
}

func TestDockerAdversarial_NilRequestFallsBackToSpawnConfig(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "ubuntu:22.04"})
	workDir := t.TempDir()

	spec, err := rt.prepareRun(SpawnConfig{
		Cmd:       []string{"env"},
		SessionID: "fallback-1234",
		TaskID:    "task-from-spawn-config",
		WorkDir:   workDir,
		Env: map[string]string{
			"VISIBLE_VAR": "from-spawn-config",
		},
		PTY: true,
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	if !hasFlagValue(spec.args, "-v", workDir+":/workspace:rw") {
		t.Fatalf("expected workdir fallback mount, got %v", spec.args)
	}
	if !hasFlagValue(spec.args, "--label", dockerTaskLabelKey+"=task-from-spawn-config") {
		t.Fatalf("expected task label from spawn config, got %v", spec.args)
	}
	if !containsArg(spec.args, "-t") {
		t.Fatalf("expected PTY fallback to add -t, got %v", spec.args)
	}

	envFile := flagValue(spec.args, "--env-file")
	if envFile == "" {
		t.Fatalf("expected --env-file in args, got %v", spec.args)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if string(data) != "VISIBLE_VAR=from-spawn-config\n" {
		t.Fatalf("expected spawn config env in env file, got %q", string(data))
	}
}

func TestDockerAdversarial_RecoverWithoutDockerDaemonReturnsGracefulError(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
if [ "$1" = "ps" ]; then
  echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2
  exit 1
fi
echo "unexpected docker command: $1" >&2
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "alpine:latest"})
	handles, err := rt.Recover(testContext(t))
	if err == nil {
		t.Fatal("expected recover to fail without a Docker daemon")
	}
	if handles != nil {
		t.Fatalf("expected no recovered handles on error, got %d", len(handles))
	}
	if !strings.Contains(err.Error(), "docker ps") {
		t.Fatalf("expected recover error to mention docker ps, got %v", err)
	}
}

func installFakeDocker(t *testing.T, script string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func dockerFlagValues(args []string, flag string) []string {
	values := make([]string, 0)
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			values = append(values, args[i+1])
		}
	}
	return values
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}
