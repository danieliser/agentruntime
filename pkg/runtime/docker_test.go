package runtime

import (
	"os"
	"path/filepath"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestDockerRuntime_Name(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{})
	if rt.Name() != "docker" {
		t.Fatalf("expected name 'docker', got %q", rt.Name())
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
	if string(data) != "VISIBLE_VAR=docker-value\n" {
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
	if !hasFlagValue(spec.args, "--network", "none") {
		t.Fatalf("expected network override in args, got %v", spec.args)
	}
	if spec.args[len(spec.args)-3] != "custom:latest" {
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
