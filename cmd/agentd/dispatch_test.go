package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDispatch_ParsesYAML(t *testing.T) {
	req, err := loadDispatchRequest(filepath.Join("..", "..", "testdata", "echo-task.yaml"))
	if err != nil {
		t.Fatalf("loadDispatchRequest returned error: %v", err)
	}

	if req.Agent != "echo-test" {
		t.Fatalf("expected agent echo-test, got %q", req.Agent)
	}
	if req.Prompt != "hello from yaml" {
		t.Fatalf("expected prompt %q, got %q", "hello from yaml", req.Prompt)
	}
	if req.WorkDir != "/tmp" {
		t.Fatalf("expected work_dir /tmp, got %q", req.WorkDir)
	}
}

func TestDispatch_ExpandsEnvVars(t *testing.T) {
	t.Setenv("PERSIST_AUTH_TOKEN", "expanded-token")

	configPath := filepath.Join(t.TempDir(), "dispatch.yaml")
	config := "agent: echo-test\nprompt: \"token=${PERSIST_AUTH_TOKEN}\"\nwork_dir: /tmp\n"
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	req, err := loadDispatchRequest(configPath)
	if err != nil {
		t.Fatalf("loadDispatchRequest returned error: %v", err)
	}

	if req.Prompt != "token=expanded-token" {
		t.Fatalf("expected expanded prompt, got %q", req.Prompt)
	}
}
