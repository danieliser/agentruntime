package runtime

import (
	"encoding/json"
	"fmt"
	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/materialize"
	"os"
	"strings"
	"testing"
)

func TestRestrictedCodexCodingUsesHostOwnedSeccomp(t *testing.T) {
	rt := NewDockerRuntime(DockerConfig{Image: "fixture"})
	rt.materializer = dockerMaterializerFunc(func(*apischema.SessionRequest, string) (*materialize.Result, error) {
		return &materialize.Result{CleanupFn: func() {}}, nil
	})
	req := &apischema.SessionRequest{Agent: "codex", WorkDir: t.TempDir(), ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}, Container: &apischema.ContainerConfig{Memory: "512m", CPUs: 1}}
	spec, err := rt.prepareRun(SpawnConfig{Cmd: []string{"fixture"}, SessionID: "coding-seccomp", WorkDir: req.WorkDir, Request: req})
	if err != nil {
		t.Fatal(err)
	}
	defer spec.cleanup()
	var profile string
	for i, arg := range spec.args {
		if arg == "--security-opt" && i+1 < len(spec.args) && strings.HasPrefix(spec.args[i+1], "seccomp=") {
			profile = strings.TrimPrefix(spec.args[i+1], "seccomp=")
		}
	}
	if profile == "" {
		t.Fatal("restricted coding worker did not receive reviewed seccomp profile")
	}
	if !hasFlagValue(spec.args, "--cap-drop", "ALL") || !hasFlagValue(spec.args, "--security-opt", "no-new-privileges:true") {
		t.Fatal("container protections lost")
	}
	if containsArg(spec.args, "--cap-add") || containsArg(spec.args, "--privileged") {
		t.Fatal("capabilities widened")
	}
	raw, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty profile")
	}
	if st, _ := os.Stat(profile); st.Mode().Perm() != 0600 {
		t.Fatal("profile not private")
	}
	if !hasFlagValue(spec.args, "--memory", "512m") || !hasFlagValue(spec.args, "--cpus", "1") {
		t.Fatal("resource controls changed")
	}
	if flagValue(spec.args, "--network") != defaultDockerNetworkName {
		t.Fatal("coding network policy changed")
	}
	env, _ := os.ReadFile(flagValue(spec.args, "--env-file"))
	if !strings.Contains(string(env), "HTTPS_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatal("managed proxy environment changed")
	}
	spec.cleanup()
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("temporary profile not cleaned")
	}
}

func TestCodexSeccompIsScopedAndRejectsCallerOverrides(t *testing.T) {
	for _, req := range []*apischema.SessionRequest{
		nil,
		{Agent: "claude", ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted"}},
		{Agent: "codex", ProviderPermissions: &apischema.ProviderPermissions{Mode: "unrestricted"}},
		{Agent: "codex", ExecutionPolicy: &apischema.ExecutionPolicy{Filesystem: "read_only"}},
	} {
		path, cleanup, err := prepareCodexSeccomp(req)
		cleanup()
		if err != nil || path != "" {
			t.Fatalf("profile affected unrelated lane: %v %s", err, path)
		}
	}
	req := &apischema.SessionRequest{Agent: "codex", ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted"}, Container: &apischema.ContainerConfig{SecurityOpt: []string{"seccomp=unconfined"}}}
	path, cleanup, err := prepareCodexSeccomp(req)
	cleanup()
	if err == nil || path != "" {
		t.Fatal("caller replaced server sandbox profile")
	}
}

func TestCodexSeccompProfileIsPinnedAndFailClosed(t *testing.T) {
	if err := validateCodexSeccompProfile(codexSeccompProfile); err != nil {
		t.Fatal(err)
	}
	mutated := append([]byte(nil), codexSeccompProfile...)
	mutated[0] = ' '
	if err := validateCodexSeccompProfile(mutated); err == nil {
		t.Fatal("altered profile accepted")
	}
	var p struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names  []string `json:"names"`
			Action string   `json:"action"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(codexSeccompProfile, &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatal("profile default widened")
	}
	denied := map[string]bool{"bpf": true, "keyctl": true, "perf_event_open": true, "kexec_load": true, "open_by_handle_at": true, "userfaultfd": true, "io_uring_setup": true}
	for _, r := range p.Syscalls {
		if r.Action == "SCMP_ACT_ALLOW" {
			for _, n := range r.Names {
				if denied[n] {
					t.Fatalf("unexpected high risk syscall allowed: %s", n)
				}
			}
		}
	}
}

func TestCodexSeccompTempfileCleanupOnPreparationFailure(t *testing.T) {
	for _, stage := range []string{"mount", "materialize"} {
		t.Run(stage, func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			rt := NewDockerRuntime(DockerConfig{Image: "fixture"})
			rt.materializer = dockerMaterializerFunc(func(*apischema.SessionRequest, string) (*materialize.Result, error) {
				return nil, fmt.Errorf("synthetic materializer failure")
			})
			req := &apischema.SessionRequest{Agent: "codex", WorkDir: t.TempDir(), ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
			cfg := SpawnConfig{Cmd: []string{"fixture"}, SessionID: "failure-cleanup", WorkDir: req.WorkDir, Request: req}
			if stage == "mount" {
				req.Mounts = []apischema.Mount{{Host: "relative-invalid", Container: "/invalid"}}
			}
			spec, err := rt.prepareRun(cfg)
			if err == nil {
				spec.cleanup()
				t.Fatal("expected preparation failure")
			}
			entries, err := os.ReadDir(temp)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "agentruntime-codex-seccomp-") {
					t.Fatal("profile tempfile leaked after failure")
				}
			}
		})
	}
}
