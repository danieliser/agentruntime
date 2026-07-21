package main

import (
	"fmt"
	"os"
	"strings"
)

// basePassthroughEnv are the host vars every spawned agent process needs to
// function, plus explicit auth and proxy wiring. Everything else on the host
// (API keys for other services, AGENT_CONFIG, tool configs) stays out.
var basePassthroughEnv = []string{
	"PATH", "HOME", "USER", "LANG", "TERM",
	"SHELL", "TMPDIR",
	// Claude OAuth (if set by the credential sync or env-file)
	"CLAUDE_CODE_OAUTH_TOKEN",
	// Agent CLI auth
	"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CURSOR_API_KEY",
	// Proxy (set by network manager)
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// hostPassthroughEnv are additionally inherited for regular (non-clean)
// sessions, where the agent is expected to see its host configuration.
// Clean-context sessions must NOT see them: XDG vars redirect config
// discovery back to host directories, NODE_OPTIONS/NODE_PATH can inject
// code into node-based CLIs, and CODEX_HOME points at the host config root.
var hostPassthroughEnv = []string{
	"XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"NODE_PATH", "NODE_OPTIONS", "NVM_DIR",
	"CODEX_HOME",
}

// buildCleanEnv creates a minimal environment for the agent process.
// Only essential system vars are inherited. No host hooks, plugins, or
// MCP servers leak through. Extra vars (e.g., IDE MCP env) are appended.
func buildCleanEnv(extra []string) []string {
	passthrough := make([]string, 0, len(basePassthroughEnv)+len(hostPassthroughEnv))
	passthrough = append(passthrough, basePassthroughEnv...)
	passthrough = append(passthrough, hostPassthroughEnv...)
	return buildEnvFrom(passthrough, extra)
}

// buildCleanContextEnv is the stricter allowlist for context:"clean"
// sessions: basePassthroughEnv only. Extras appended last win over
// passthrough entries when os/exec dedups the env (later values are kept),
// which is how HOME/CODEX_HOME overrides for ephemeral homes take effect.
func buildCleanContextEnv(extra []string) []string {
	return buildEnvFrom(basePassthroughEnv, extra)
}

func buildEnvFrom(passthrough, extra []string) []string {
	env := make([]string, 0, len(passthrough)+len(extra))
	hostEnv := make(map[string]string)
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			hostEnv[e[:i]] = e[i+1:]
		}
	}

	for _, key := range passthrough {
		if val, ok := hostEnv[key]; ok {
			env = append(env, key+"="+val)
		}
	}

	// NOTE: In Docker containers, HOME=/home/agent with our clean .claude/ mount.
	// When running locally, Claude reads the host's ~/.claude/ (hooks, plugins, etc).
	// This is expected — the Docker container IS the isolation boundary.

	// Append explicit extras (MCP server env vars, ephemeral home overrides, etc.)
	env = append(env, extra...)

	return env
}

// redactPromptArgs returns a copy of args with any element equal to the
// prompt replaced by a placeholder. Spawn logging must never persist prompt
// text — prompts can carry pasted tokens, signed URLs, or proprietary data.
func redactPromptArgs(args []string, prompt string) []string {
	if prompt == "" {
		return args
	}
	out := append([]string(nil), args...)
	for i, arg := range out {
		if arg == prompt {
			out[i] = fmt.Sprintf("[prompt: %d bytes]", len(prompt))
		}
	}
	return out
}
