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

func TestResources_BackwardCompat(t *testing.T) {
	// Resources is a type alias for ContainerConfig — verify it works
	var r Resources
	r.Image = "test:latest"
	r.Memory = "2g"
	if r.Image != "test:latest" {
		t.Errorf("Resources.Image = %q, want %q", r.Image, "test:latest")
	}
}
