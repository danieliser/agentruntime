package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	runtimepkg "github.com/danieliser/agentruntime/pkg/runtime"
)

const (
	helperProcessEnv = "AGENTRUNTIME_HELPER_PROCESS"
	helperEchoEnv    = "AGENTRUNTIME_HELPER_ENV"
)

type injectionAgentCase struct {
	name                 string
	agent                Agent
	supportsSessionID    bool
	supportsAllowedTools bool
}

func TestBuildCmd_PromptMetacharacters_AllAgents(t *testing.T) {
	prompt := "keep literal ; && || | > < $() `echo-nope`"

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd(prompt, AgentConfig{})
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			assertArgPresentOnce(t, cmd, prompt)
			assertNoShC(t, cmd)
		})
	}
}

func TestBuildCmd_PromptControlCharacters_AllAgents(t *testing.T) {
	prompt := "line1\nline2\x00\x01\x1f\t\r"

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd(prompt, AgentConfig{})
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			assertArgPresentOnce(t, cmd, prompt)
			assertNoShC(t, cmd)
		})
	}
}

func TestBuildCmd_ModelValueStartingWithFlag_AllAgents(t *testing.T) {
	model := "--dangerous-flag"

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd("safe prompt", AgentConfig{Model: model})
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			assertFlagValue(t, cmd, "--model", model)
			assertNoShC(t, cmd)
		})
	}
}

func TestBuildCmd_SessionIDPathTraversal_AllAgents(t *testing.T) {
	sessionID := "../../etc/passwd"

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd("resume safely", AgentConfig{SessionID: sessionID})
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			if tc.supportsSessionID {
				assertFlagValue(t, cmd, "--session-id", sessionID)
				if !contains(cmd, "--resume") {
					t.Fatalf("expected --resume in cmd, got %v", cmd)
				}
			} else if contains(cmd, sessionID) {
				t.Fatalf("unexpected session id in unsupported agent cmd: %v", cmd)
			}

			assertNoShC(t, cmd)
		})
	}
}

func TestBuildCmd_AllowedToolsShellEscapes_AllAgents(t *testing.T) {
	tools := []string{
		"Read;touch /tmp/pwned",
		"Write && whoami",
		"$(uname -a)",
		"`id`",
	}

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd("tool check", AgentConfig{AllowedTools: tools})
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			if tc.supportsAllowedTools {
				for _, tool := range tools {
					assertFlagValue(t, cmd, "--allowedTools", tool)
				}
			} else {
				for _, tool := range tools {
					if contains(cmd, tool) {
						t.Fatalf("unexpected allowed tool in unsupported agent cmd: %v", cmd)
					}
				}
			}

			assertNoShC(t, cmd)
		})
	}
}

func TestBuildCmd_EnvDoesNotLeakIntoCmd_AllAgents(t *testing.T) {
	cfg := AgentConfig{
		Env: map[string]string{
			"DOCKER_ESCAPE": "\"; --privileged --entrypoint sh; $(uname); `id` #",
			"JSON_PAYLOAD":  "{\"cmd\":\"$(touch /tmp/pwned)\"}",
		},
	}

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd("env should stay out of argv", cfg)
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			for key, value := range cfg.Env {
				assertArgAbsent(t, cmd, key)
				assertArgAbsent(t, cmd, value)
				assertArgAbsent(t, cmd, key+"="+value)
			}

			assertNoShC(t, cmd)
		})
	}
}

func TestBuildCmd_NeverUsesShC_AllAgents(t *testing.T) {
	cfg := AgentConfig{
		Model:        "--dangerous-flag",
		SessionID:    "../../etc/passwd",
		AllowedTools: []string{"$(touch /tmp/pwned)", "`id`"},
	}
	prompt := "line1\nline2 ; && || | > < $() `echo-nope`"

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd(prompt, cfg)
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			assertNoShC(t, cmd)
		})
	}
}

func TestBuildCmd_ExtremelyLongStrings_AllAgents(t *testing.T) {
	model := strings.Repeat("m", 1<<20)
	prompt := strings.Repeat("p", 1<<20)

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("BuildCmd panicked with long inputs: %v", r)
				}
			}()

			cmd, err := tc.agent.BuildCmd(prompt, AgentConfig{Model: model})
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			assertArgPresentOnce(t, cmd, prompt)
			assertFlagValue(t, cmd, "--model", model)
			assertNoShC(t, cmd)
		})
	}
}

func TestLocalRuntime_SpawnPreservesMaliciousPrompt_AllAgents(t *testing.T) {
	requirePOSIXHelperHarness(t)
	installHelperAgentBinaries(t)

	rt := runtimepkg.NewLocalRuntime()
	prompt := "argv literal '\" ; && || | > < $() `echo-nope`"

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.agent.BuildCmd(prompt, AgentConfig{})
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			handle, err := rt.Spawn(ctx, runtimepkg.SpawnConfig{
				AgentName: tc.name,
				Cmd:       cmd,
				Env: map[string]string{
					helperProcessEnv: "1",
				},
			})
			if err != nil {
				t.Fatalf("spawn failed: %v", err)
			}
			defer killAndWait(t, handle)

			if handle.PID() <= 0 {
				t.Fatalf("expected positive pid, got %d", handle.PID())
			}

			cmdline := waitForPSCommand(t, handle.PID(), prompt)
			if !strings.Contains(cmdline, prompt) {
				t.Fatalf("expected prompt %q in process command line %q", prompt, cmdline)
			}
		})
	}
}

func TestLocalRuntime_SpawnPreservesMaliciousEnv_AllAgents(t *testing.T) {
	requirePOSIXHelperHarness(t)
	installHelperAgentBinaries(t)

	rt := runtimepkg.NewLocalRuntime()
	envValue := "\"; --privileged --entrypoint sh; $(uname); `id` #"

	for _, tc := range injectionAgentCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfg := AgentConfig{
				Env: map[string]string{
					helperProcessEnv: "1",
					helperEchoEnv:    envValue,
				},
			}

			cmd, err := tc.agent.BuildCmd("env check", cfg)
			if err != nil {
				t.Fatalf("BuildCmd returned error: %v", err)
			}
			assertArgAbsent(t, cmd, envValue)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			handle, err := rt.Spawn(ctx, runtimepkg.SpawnConfig{
				AgentName: tc.name,
				Cmd:       cmd,
				Env:       cfg.Env,
			})
			if err != nil {
				t.Fatalf("spawn failed: %v", err)
			}
			defer killAndWait(t, handle)

			reader := bufio.NewReader(handle.Stdout())
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read helper env line failed: %v", err)
			}

			got := strings.TrimSuffix(line, "\n")
			want := "ENV:" + envValue
			if got != want {
				t.Fatalf("expected helper env %q, got %q", want, got)
			}
		})
	}
}

func TestAgentInjectionHelperProcess(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		return
	}

	fmt.Fprintf(os.Stdout, "ENV:%s\n", os.Getenv(helperEchoEnv))
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func injectionAgentCases() []injectionAgentCase {
	return []injectionAgentCase{
		{
			name:                 "claude",
			agent:                &ClaudeAgent{},
			supportsSessionID:    true,
			supportsAllowedTools: true,
		},
		{
			name:  "codex",
			agent: &CodexAgent{},
		},
		{
			name:  "opencode",
			agent: &OpenCodeAgent{},
		},
	}
}

func assertArgPresentOnce(t *testing.T, cmd []string, want string) {
	t.Helper()

	count := 0
	for _, arg := range cmd {
		if arg == want {
			count++
		}
	}

	if count != 1 {
		t.Fatalf("expected %q exactly once in cmd, got %d occurrences in %v", want, count, cmd)
	}
}

func assertArgAbsent(t *testing.T, cmd []string, want string) {
	t.Helper()

	for _, arg := range cmd {
		if arg == want {
			t.Fatalf("did not expect %q in cmd %v", want, cmd)
		}
	}
}

func assertFlagValue(t *testing.T, cmd []string, flag, want string) {
	t.Helper()

	matches := 0
	for i := 0; i < len(cmd)-1; i++ {
		if cmd[i] == flag && cmd[i+1] == want {
			matches++
		}
	}

	if matches == 0 {
		t.Fatalf("expected %s %q in cmd, got %v", flag, want, cmd)
	}

	occurrences := 0
	for _, arg := range cmd {
		if arg == want {
			occurrences++
		}
	}
	if occurrences != matches {
		t.Fatalf("expected %q to appear only as value for %s in %v", want, flag, cmd)
	}
}

func assertNoShC(t *testing.T, cmd []string) {
	t.Helper()

	for i := 0; i < len(cmd)-1; i++ {
		if cmd[i] == "sh" && cmd[i+1] == "-c" {
			t.Fatalf("unexpected shell execution in cmd: %v", cmd)
		}
	}
}

func requirePOSIXHelperHarness(t *testing.T) {
	t.Helper()

	if stdruntime.GOOS == "windows" {
		t.Skip("helper binary harness requires POSIX shell wrappers")
	}
}

func installHelperAgentBinaries(t *testing.T) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary failed: %v", err)
	}

	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestAgentInjectionHelperProcess -- \"$@\"\n", shellSingleQuote(exe))

	for _, name := range []string{"claude", "codex", "opencode"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write helper binary %s failed: %v", name, err)
		}
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func waitForPSCommand(t *testing.T, pid int, want string) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmdline, err := psCommandLine(pid)
		if err == nil && strings.Contains(cmdline, want) {
			return cmdline
		}
		time.Sleep(50 * time.Millisecond)
	}

	cmdline, err := psCommandLine(pid)
	if err != nil {
		t.Fatalf("ps lookup for pid %d failed: %v", pid, err)
	}
	t.Fatalf("expected prompt %q in ps output %q", want, cmdline)
	return ""
}

func psCommandLine(pid int) (string, error) {
	// -ww disables ps truncation so we can assert the full argv string.
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func killAndWait(t *testing.T, handle runtimepkg.ProcessHandle) {
	t.Helper()

	if handle == nil {
		return
	}

	_ = handle.Kill()

	select {
	case <-handle.Wait():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for helper process to exit")
	}
}
