package runtime

import (
	"encoding/json"
	"fmt"
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

	got := args[len(args)-1]
	if got != image {
		t.Fatalf("expected image %q, got %q in args %v", image, got, args)
	}
}

func TestDockerAdversarial_InvalidMemoryFailsGracefully(t *testing.T) {
	installFakeDocker(t, `#!/bin/sh
set -eu
case "$1" in
  network)
    if [ "$2" = "inspect" ]; then
      echo "Error: No such network: agentruntime-agents" >&2
      exit 1
    fi
    if [ "$2" = "create" ]; then
      exit 0
    fi
    ;;
  inspect)
    if [ "$2" = "--format" ]; then
      echo "Error: No such object: agentruntime-proxy" >&2
      exit 1
    fi
    ;;
  run)
    shift
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --name)
          if [ "$2" = "agentruntime-proxy" ]; then
            printf '%s\n' "proxy-container"
            exit 0
          fi
          shift 2
          ;;
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
    ;;
esac
echo "unexpected docker command: $*" >&2
exit 2
`)

	rt := NewDockerRuntime(DockerConfig{Image: "alpine:latest"})
	_, err := rt.Spawn(testContext(t), SpawnConfig{
		Cmd:       []string{"echo", "ignored"},
		SessionID: "invalid-memory-1234",
		Request: &apischema.SessionRequest{
			Container: &apischema.ContainerConfig{Memory: "lots"},
		},
	})
	if err == nil {
		t.Fatal("expected spawn to fail when docker rejects the memory limit")
	}
	if !strings.Contains(err.Error(), "docker run") {
		t.Fatalf("expected error to mention docker run, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid memory value") {
		t.Fatalf("expected docker error to mention invalid memory value, got %v", err)
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
	sidecarPort := startFakeDockerSidecar(t)
	stateDir := t.TempDir()
	installFakeDocker(t, fmt.Sprintf(`#!/bin/sh
set -eu
state_dir=%q
sidecar_port=%q
case "$1" in
  network)
    case "$2" in
      inspect)
        if [ -e "$state_dir/.network" ]; then
          exit 0
        fi
        echo "Error: No such network: agentruntime-agents" >&2
        exit 1
        ;;
      create)
        : >"$state_dir/.network"
        exit 0
        ;;
    esac
    ;;
  inspect)
    if [ "$2" = "--format" ]; then
      if [ -e "$state_dir/.proxy" ]; then
        printf 'true\n'
        exit 0
      fi
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
    if [ -z "$name" ]; then
      echo "missing container name" >&2
      exit 2
    fi
    if [ "$name" = "agentruntime-proxy" ]; then
      : >"$state_dir/.proxy"
      printf 'proxy-container\n'
      exit 0
    fi
    marker="$state_dir/$name"
    if [ -e "$marker" ]; then
      echo "docker: Error response from daemon: Conflict. The container name \"/$name\" is already in use." >&2
      exit 125
    fi
    : >"$marker"
    printf '%%s\n' "$name"
    ;;
  port)
    printf '0.0.0.0:%%s\n' "$sidecar_port"
    ;;
  stop|rm)
    rm -f "$state_dir/$2"
    ;;
  *)
    echo "unexpected docker command: $1" >&2
    exit 2
    ;;
esac
`, stateDir, sidecarPort))

	rt := NewDockerRuntime(DockerConfig{Image: "alpine:latest"})
	firstCfg := SpawnConfig{
		Cmd:       []string{"echo", "first"},
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
		if !strings.Contains(err.Error(), "docker run") {
			t.Fatalf("expected docker run error for collision, got %v", err)
		}
		return
	}
	if firstName == secondName {
		t.Fatalf("expected duplicate container name %q to fail, but spawn succeeded", firstName)
	}
	t.Cleanup(func() {
		_ = second.Kill()
	})
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

	got := args[len(args)-1]
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
	contents := string(data)
	if !strings.Contains(contents, "AGENT_CMD=[\"env\"]\n") {
		t.Fatalf("expected AGENT_CMD in env file, got %q", contents)
	}
	if !strings.Contains(contents, "VISIBLE_VAR=from-spawn-config\n") {
		t.Fatalf("expected spawn config env in env file, got %q", contents)
	}
	if !strings.Contains(contents, "HTTP_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected proxy env in env file, got %q", contents)
	}
	if !strings.Contains(contents, "HTTPS_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected proxy env in env file, got %q", contents)
	}
	if !strings.Contains(contents, "NO_PROXY=localhost,127.0.0.1,host.docker.internal\n") {
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

func startFakeDockerSidecar(t *testing.T) string {
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
			for {
				var frame wsClientFrame
				if err := conn.ReadJSON(&frame); err != nil {
					return
				}
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
