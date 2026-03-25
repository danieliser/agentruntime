package schema

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEffectiveMounts_Empty(t *testing.T) {
	req := SessionRequest{}
	mounts := req.EffectiveMounts()
	if len(mounts) != 0 {
		t.Fatalf("expected 0 mounts, got %d", len(mounts))
	}
}

func TestEffectiveMounts_WorkDirOnly(t *testing.T) {
	req := SessionRequest{WorkDir: "/home/user/project"}
	mounts := req.EffectiveMounts()
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].Host != "/home/user/project" {
		t.Errorf("host = %q, want %q", mounts[0].Host, "/home/user/project")
	}
	if mounts[0].Container != "/workspace" {
		t.Errorf("container = %q, want %q", mounts[0].Container, "/workspace")
	}
	if mounts[0].Mode != "rw" {
		t.Errorf("mode = %q, want %q", mounts[0].Mode, "rw")
	}
}

func TestEffectiveMounts_WorkDirPlusMounts(t *testing.T) {
	req := SessionRequest{
		WorkDir: "/project",
		Mounts: []Mount{
			{Host: "/data", Container: "/mnt/data", Mode: "ro"},
		},
	}
	mounts := req.EffectiveMounts()
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	// WorkDir should be first
	if mounts[0].Container != "/workspace" {
		t.Errorf("first mount container = %q, want /workspace", mounts[0].Container)
	}
	if mounts[1].Host != "/data" {
		t.Errorf("second mount host = %q, want /data", mounts[1].Host)
	}
}

func TestEffectiveMounts_DoesNotMutateOriginal(t *testing.T) {
	original := []Mount{{Host: "/a", Container: "/b", Mode: "ro"}}
	req := SessionRequest{WorkDir: "/project", Mounts: original}
	_ = req.EffectiveMounts()
	if len(original) != 1 {
		t.Fatal("EffectiveMounts mutated the original Mounts slice")
	}
}

func TestEffectiveTimeout_Default(t *testing.T) {
	req := SessionRequest{}
	if d := req.EffectiveTimeout(); d != 5*time.Minute {
		t.Errorf("default timeout = %v, want 5m", d)
	}
}

func TestEffectiveTimeout_Valid(t *testing.T) {
	req := SessionRequest{Timeout: "30s"}
	if d := req.EffectiveTimeout(); d != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", d)
	}
}

func TestEffectiveTimeout_Invalid(t *testing.T) {
	req := SessionRequest{Timeout: "not-a-duration"}
	if d := req.EffectiveTimeout(); d != 5*time.Minute {
		t.Errorf("invalid timeout = %v, want 5m fallback", d)
	}
}

func TestSessionRequest_JSONRoundTrip(t *testing.T) {
	req := SessionRequest{
		Agent:   "claude",
		Prompt:  "hello",
		WorkDir: "/project",
		Model:   "claude-opus-4-5",
		Timeout: "10m",
		Claude: &ClaudeConfig{
			MaxTurns:     5,
			AllowedTools: []string{"Read", "Write"},
		},
		Container: &ContainerConfig{
			Image:  "ubuntu:22.04",
			Memory: "4g",
			CPUs:   2.0,
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SessionRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Agent != req.Agent {
		t.Errorf("agent = %q, want %q", decoded.Agent, req.Agent)
	}
	if decoded.Model != req.Model {
		t.Errorf("model = %q, want %q", decoded.Model, req.Model)
	}
	if decoded.Claude == nil {
		t.Fatal("claude config is nil after round-trip")
	}
	if decoded.Claude.MaxTurns != 5 {
		t.Errorf("max_turns = %d, want 5", decoded.Claude.MaxTurns)
	}
	if decoded.Container == nil {
		t.Fatal("container config is nil after round-trip")
	}
	if decoded.Container.CPUs != 2.0 {
		t.Errorf("cpus = %f, want 2.0", decoded.Container.CPUs)
	}
}

func TestParseVolumes(t *testing.T) {
	tests := []struct {
		name    string
		volumes []string
		want    []Mount
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"host:container", []string{"/host:/container"}, []Mount{
			{Host: "/host", Container: "/container", Mode: "rw"},
		}},
		{"host:container:ro", []string{"/host:/container:ro"}, []Mount{
			{Host: "/host", Container: "/container", Mode: "ro"},
		}},
		{"multiple", []string{"/a:/b:rw", "/c:/d:ro"}, []Mount{
			{Host: "/a", Container: "/b", Mode: "rw"},
			{Host: "/c", Container: "/d", Mode: "ro"},
		}},
		{"malformed skipped", []string{"no-colon", "/a:/b"}, []Mount{
			{Host: "/a", Container: "/b", Mode: "rw"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := SessionRequest{Volumes: tt.volumes}
			got := req.ParseVolumes()
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i, m := range got {
				if m.Host != tt.want[i].Host || m.Container != tt.want[i].Container || m.Mode != tt.want[i].Mode {
					t.Errorf("[%d] got %+v, want %+v", i, m, tt.want[i])
				}
			}
		})
	}
}

func TestEffectiveMounts_IncludesVolumes(t *testing.T) {
	req := SessionRequest{
		WorkDir: "/project",
		Volumes: []string{"/hooks:/hooks:ro"},
	}
	mounts := req.EffectiveMounts()
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	// WorkDir first, then volume
	if mounts[0].Container != "/workspace" {
		t.Errorf("first mount = %q, want /workspace", mounts[0].Container)
	}
	if mounts[1].Host != "/hooks" || mounts[1].Container != "/hooks" || mounts[1].Mode != "ro" {
		t.Errorf("volume mount = %+v", mounts[1])
	}
}

func TestLifecycleConfig_JSONRoundTrip(t *testing.T) {
	req := SessionRequest{
		Agent:  "claude",
		Prompt: "hello",
		Lifecycle: &LifecycleConfig{
			PreInit:     "/hooks/setup.sh",
			PostInit:    "/hooks/warmup.sh",
			Sidecar:     "/hooks/watchdog.sh",
			PostRun:     "/hooks/cleanup.sh",
			HookTimeout: 15,
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SessionRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Lifecycle == nil {
		t.Fatal("lifecycle is nil after round-trip")
	}
	if decoded.Lifecycle.PreInit != "/hooks/setup.sh" {
		t.Errorf("pre_init = %q", decoded.Lifecycle.PreInit)
	}
	if decoded.Lifecycle.Sidecar != "/hooks/watchdog.sh" {
		t.Errorf("sidecar = %q", decoded.Lifecycle.Sidecar)
	}
	if decoded.Lifecycle.HookTimeout != 15 {
		t.Errorf("hook_timeout = %d, want 15", decoded.Lifecycle.HookTimeout)
	}
}

func TestResources_BackwardCompat(t *testing.T) {
	// Resources is a type alias for ContainerConfig — verify it works
	var r Resources
	r.Image = "test:latest"
	r.Memory = "2g"
	if r.Image != "test:latest" {
		t.Errorf("Resources.Image = %q, want %q", r.Image, "test:latest")
	}
}
