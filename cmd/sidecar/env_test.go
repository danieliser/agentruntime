package main

import (
	"strings"
	"testing"
)

func envHasKey(env []string, key string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return true
		}
	}
	return false
}

func TestBuildCleanEnv_AllowlistOnly(t *testing.T) {
	t.Setenv("AGENTRUNTIME_TEST_SECRET", "leak-me")
	t.Setenv("AGENT_CONFIG", `{"context":"clean"}`)
	t.Setenv("NODE_OPTIONS", "--require /tmp/evil.js")
	t.Setenv("XDG_CONFIG_HOME", "/host/config")

	env := buildCleanEnv(nil)

	if !envHasKey(env, "PATH") || !envHasKey(env, "HOME") {
		t.Fatalf("clean env missing PATH/HOME: %v", env)
	}
	for _, key := range []string{"AGENTRUNTIME_TEST_SECRET", "AGENT_CONFIG"} {
		if envHasKey(env, key) {
			t.Fatalf("host var %s leaked into clean env: %v", key, env)
		}
	}
	// Regular sessions still see host node/XDG wiring.
	if !envHasKey(env, "NODE_OPTIONS") || !envHasKey(env, "XDG_CONFIG_HOME") {
		t.Fatalf("non-clean env should pass through NODE_OPTIONS/XDG_CONFIG_HOME: %v", env)
	}
}

func TestBuildCleanContextEnv_StripsInjectionVectors(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--require /tmp/evil.js")
	t.Setenv("NODE_PATH", "/host/node_modules")
	t.Setenv("XDG_CONFIG_HOME", "/host/config")
	t.Setenv("XDG_DATA_HOME", "/host/data")
	t.Setenv("CODEX_HOME", "/host/.codex")
	t.Setenv("AGENTRUNTIME_TEST_SECRET", "leak-me")

	env := buildCleanContextEnv([]string{"HOME=/tmp/fake-home"})

	if !envHasKey(env, "PATH") {
		t.Fatalf("clean-context env missing PATH: %v", env)
	}
	for _, key := range []string{
		"NODE_OPTIONS", "NODE_PATH", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"CODEX_HOME", "AGENTRUNTIME_TEST_SECRET",
	} {
		if envHasKey(env, key) {
			t.Fatalf("clean-context env must strip %s: %v", key, env)
		}
	}
	// The extra override is appended last so it wins after os/exec dedup.
	if env[len(env)-1] != "HOME=/tmp/fake-home" {
		t.Fatalf("extras must be appended last, env = %v", env)
	}
}

func TestRedactPromptArgs(t *testing.T) {
	prompt := "summarize sk-secret-token contents"
	args := []string{"--print", "-p", prompt, "--force"}

	got := redactPromptArgs(args, prompt)

	for _, arg := range got {
		if arg == prompt {
			t.Fatalf("prompt not redacted: %v", got)
		}
	}
	if got[1] != "-p" || got[3] != "--force" {
		t.Fatalf("non-prompt args must be preserved: %v", got)
	}
	// Original slice untouched.
	if args[2] != prompt {
		t.Fatalf("redactPromptArgs must not mutate its input: %v", args)
	}
	if redacted := redactPromptArgs(args, ""); &redacted[0] != &args[0] {
		t.Fatalf("empty prompt should return args unchanged")
	}
}
