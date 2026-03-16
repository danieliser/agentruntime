package materialize

import (
	"strings"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func FuzzResolveVars(f *testing.F) {
	f.Add("")
	f.Add("http://example.test:8080")
	f.Add("http://${HOST_GATEWAY}:8080")
	f.Add("${HOST_GATEWAY}")
	f.Add("prefix-${HOST_GATEWAY}-suffix")
	f.Add("${HOST_GATEWAY}${HOST_GATEWAY}")
	f.Add("$HOST_GATEWAY")
	f.Add("\x00${HOST_GATEWAY}\xff")

	f.Fuzz(func(t *testing.T, input string) {
		got := ResolveVars(input)

		if !strings.Contains(input, hostGatewayVar) {
			if got != input {
				t.Fatalf("expected unchanged string %q, got %q", input, got)
			}
			return
		}

		if strings.Contains(got, hostGatewayVar) {
			t.Fatalf("expected placeholder to be resolved in %q", got)
		}
	})
}

func FuzzMaterialize(f *testing.F) {
	f.Add(
		"claude",
		"local",
		"do the thing",
		"/tmp/workspace",
		"5m",
		"session-12345678",
		"# team instructions\nuse rg\n",
		"Follow repo conventions.\n",
		"${HOME}/.claude/credentials.json",
		"${HOME}/.claude/projects",
		"http://${HOST_GATEWAY}:8080",
		"/host/path",
		"/container/path",
		"rw",
		"project",
		"agentruntime",
		true,
		2.0,
	)
	f.Add(
		"codex",
		"docker",
		"",
		"",
		"not-a-duration",
		"",
		"",
		"",
		"~/.claude/credentials.json",
		"~/.claude/projects",
		"",
		"",
		"",
		"ro",
		"",
		"",
		false,
		-1.0,
	)
	f.Add(
		"",
		"",
		"",
		"",
		"",
		"../../bad/session/id",
		"",
		"",
		"",
		"",
		"http://example.test",
		"",
		"",
		"",
		"",
		"",
		false,
		0.0,
	)

	f.Fuzz(func(
		t *testing.T,
		agent string,
		runtimeName string,
		prompt string,
		workDir string,
		timeout string,
		sessionID string,
		claudeMD string,
		codexInstructions string,
		credentialsPath string,
		memoryPath string,
		mcpURL string,
		mountHost string,
		mountContainer string,
		mountMode string,
		tagKey string,
		tagValue string,
		pty bool,
		cpus float64,
	) {
		req := &apischema.SessionRequest{
			Agent:   agent,
			Runtime: runtimeName,
			Prompt:  prompt,
			Timeout: timeout,
			PTY:     pty,
			WorkDir: workDir,
			Mounts: []apischema.Mount{
				{
					Host:      mountHost,
					Container: mountContainer,
					Mode:      mountMode,
				},
			},
			Container: &apischema.ContainerConfig{
				Image:   runtimeName,
				CPUs:    cpus,
				Network: "bridge",
			},
		}

		if tagKey != "" {
			req.Tags = map[string]string{tagKey: tagValue}
			req.Env = map[string]string{"FUZZ_ENV": tagValue}
		}

		if agent != "" || claudeMD != "" || credentialsPath != "" || memoryPath != "" || mcpURL != "" {
			req.Claude = &apischema.ClaudeConfig{
				SettingsJSON: map[string]any{
					"agent": agent,
					"pty":   pty,
				},
				ClaudeMD:        claudeMD,
				CredentialsPath: credentialsPath,
				MemoryPath:      memoryPath,
				OutputFormat:    "stream-json",
			}
		}

		if runtimeName != "" || codexInstructions != "" {
			req.Codex = &apischema.CodexConfig{
				ConfigTOML: map[string]any{
					"model":   agent,
					"runtime": runtimeName,
					"cpus":    cpus,
					"modes":   []string{"suggest", "full-auto"},
				},
				Instructions: codexInstructions,
				ApprovalMode: "suggest",
			}
		}

		if mcpURL != "" {
			req.MCPServers = []apischema.MCPServer{
				{
					Name:  "seed",
					Type:  "http",
					URL:   mcpURL,
					Token: tagValue,
					Env:   map[string]string{"FUZZ_ENV": tagValue},
				},
			}
		}

		result, err := Materialize(req, sessionID)
		if err != nil {
			return
		}
		if result == nil {
			t.Fatal("expected non-nil result on success")
		}
		if result.CleanupFn == nil {
			t.Fatal("expected cleanup function on success")
		}
		result.CleanupFn()
	})
}
