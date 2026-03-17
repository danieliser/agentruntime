package runtime

import (
	"encoding/json"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestBuildAgentConfigJSON_Empty(t *testing.T) {
	got := buildAgentConfigJSON(SpawnConfig{})
	if got != "" {
		t.Fatalf("expected empty string for zero SpawnConfig, got %q", got)
	}
}

func TestBuildAgentConfigJSON_NilRequest(t *testing.T) {
	cfg := SpawnConfig{Model: "claude-opus-4-5"}
	got := buildAgentConfigJSON(cfg)
	if got == "" {
		t.Fatal("expected non-empty JSON when Model is set")
	}
	var ac sidecarAgentConfig
	if err := json.Unmarshal([]byte(got), &ac); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ac.Model != "claude-opus-4-5" {
		t.Fatalf("Model = %q, want %q", ac.Model, "claude-opus-4-5")
	}
}

func TestBuildAgentConfigJSON_RequestFields(t *testing.T) {
	cfg := SpawnConfig{
		Request: &apischema.SessionRequest{
			Model:         "claude-sonnet-4-5",
			ResumeSession: "sess-123",
			Env:           map[string]string{"FOO": "bar"},
			Claude: &apischema.ClaudeConfig{
				MaxTurns:     10,
				AllowedTools: []string{"Read", "Write"},
			},
		},
	}
	got := buildAgentConfigJSON(cfg)
	var ac sidecarAgentConfig
	if err := json.Unmarshal([]byte(got), &ac); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ac.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want %q", ac.Model, "claude-sonnet-4-5")
	}
	if ac.ResumeSession != "sess-123" {
		t.Errorf("ResumeSession = %q, want %q", ac.ResumeSession, "sess-123")
	}
	if ac.MaxTurns != 10 {
		t.Errorf("MaxTurns = %d, want 10", ac.MaxTurns)
	}
	if len(ac.AllowedTools) != 2 || ac.AllowedTools[0] != "Read" {
		t.Errorf("AllowedTools = %v, want [Read Write]", ac.AllowedTools)
	}
	if ac.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want %q", ac.Env["FOO"], "bar")
	}
}

func TestBuildAgentConfigJSON_CodexApprovalMode(t *testing.T) {
	cfg := SpawnConfig{
		Request: &apischema.SessionRequest{
			Codex: &apischema.CodexConfig{
				ApprovalMode: "full-auto",
			},
		},
	}
	got := buildAgentConfigJSON(cfg)
	var ac sidecarAgentConfig
	if err := json.Unmarshal([]byte(got), &ac); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ac.ApprovalMode != "full-auto" {
		t.Errorf("ApprovalMode = %q, want %q", ac.ApprovalMode, "full-auto")
	}
}

func TestBuildAgentConfigJSON_SpawnModelOverridesRequest(t *testing.T) {
	cfg := SpawnConfig{
		Model: "override-model",
		Request: &apischema.SessionRequest{
			Model: "request-model",
		},
	}
	got := buildAgentConfigJSON(cfg)
	var ac sidecarAgentConfig
	if err := json.Unmarshal([]byte(got), &ac); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ac.Model != "override-model" {
		t.Errorf("Model = %q, want %q (SpawnConfig.Model should override Request.Model)", ac.Model, "override-model")
	}
}

func TestBuildAgentConfigJSON_EmptyRequestFields(t *testing.T) {
	// Request present but all fields empty — should return empty string.
	cfg := SpawnConfig{
		Request: &apischema.SessionRequest{},
	}
	got := buildAgentConfigJSON(cfg)
	if got != "" {
		t.Fatalf("expected empty string for empty SessionRequest, got %q", got)
	}
}
