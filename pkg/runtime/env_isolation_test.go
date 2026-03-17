package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRuntimeEnvHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	switch os.Getenv("HELPER_ACTION") {
	case "print-env":
		for _, entry := range os.Environ() {
			fmt.Fprintln(os.Stdout, entry)
		}
	case "print-var":
		io.WriteString(os.Stdout, os.Getenv(os.Getenv("HELPER_TARGET")))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper action %q", os.Getenv("HELPER_ACTION"))
		os.Exit(2)
	}

	os.Exit(0)
}

// TestLocalRuntime_InheritsParentEnv verifies that the local runtime inherits
// the parent process environment. This is correct for local — isolation is
// the Docker runtime's job. Local subprocesses need PATH, HOME, etc. to work.
func TestLocalRuntime_InheritsParentEnv(t *testing.T) {
	t.Setenv("SECRET_KEY", "daemon-secret")

	rt := NewLocalRuntime()
	handle, err := rt.Spawn(testContext(t), localHelperConfig("print-var", map[string]string{
		"HELPER_TARGET": "SECRET_KEY",
	}))
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	stdout, stderr, result := readProcessOutput(t, handle)
	if result.Err != nil {
		t.Fatalf("wait failed: %v (stderr=%q)", result.Err, stderr)
	}
	if result.Code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%q)", result.Code, stderr)
	}
	if stdout != "daemon-secret" {
		t.Fatalf("expected SECRET_KEY to be inherited as 'daemon-secret', got %q", stdout)
	}
}

func TestLocalRuntime_ExplicitEnvIsAvailable(t *testing.T) {
	rt := NewLocalRuntime()
	handle, err := rt.Spawn(testContext(t), localHelperConfig("print-var", map[string]string{
		"HELPER_TARGET": "VISIBLE_VAR",
		"VISIBLE_VAR":   "hello-local",
	}))
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	stdout, stderr, result := readProcessOutput(t, handle)
	if result.Err != nil {
		t.Fatalf("wait failed: %v (stderr=%q)", result.Err, stderr)
	}
	if result.Code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%q)", result.Code, stderr)
	}
	if stdout != "hello-local" {
		t.Fatalf("expected explicit env value, got %q", stdout)
	}
}

func TestLocalRuntime_RejectsReservedEnvOverrides(t *testing.T) {
	rt := NewLocalRuntime()
	_, err := rt.Spawn(testContext(t), localHelperConfig("print-env", map[string]string{
		"PATH": "/tmp/malicious",
	}))
	if err == nil {
		t.Fatal("expected PATH override to be rejected")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved env error, got %v", err)
	}
}

func TestLocalRuntime_PassesMetacharactersLiterally(t *testing.T) {
	rt := NewLocalRuntime()
	want := "`uname` $(whoami) ${HOME} && echo literal"

	handle, err := rt.Spawn(testContext(t), SpawnConfig{
		Cmd: []string{"sh", "-c", "printf %s \"$TEST_VALUE\""},
		Env: map[string]string{"TEST_VALUE": want},
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	stdout, stderr, result := readProcessOutput(t, handle)
	if result.Err != nil {
		t.Fatalf("wait failed: %v (stderr=%q)", result.Err, stderr)
	}
	if result.Code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%q)", result.Code, stderr)
	}
	if stdout != want {
		t.Fatalf("expected literal value %q, got %q", want, stdout)
	}
}

func TestLocalRuntime_InvalidEnvKeysError(t *testing.T) {
	rt := NewLocalRuntime()

	cases := []struct {
		name string
		key  string
	}{
		{name: "equals", key: "BAD=KEY"},
		{name: "space", key: "BAD KEY"},
		{name: "newline", key: "BAD\nKEY"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.Spawn(testContext(t), localHelperConfig("print-env", map[string]string{
				tc.key: "value",
			}))
			if err == nil {
				t.Fatalf("expected invalid env key %q to fail", tc.key)
			}
			if !strings.Contains(err.Error(), "invalid env key") {
				t.Fatalf("expected invalid env key error, got %v", err)
			}
		})
	}
}

func TestLocalRuntime_EmptyEnvMapDoesNotPanic(t *testing.T) {
	rt := NewLocalRuntime()
	handle, err := rt.Spawn(testContext(t), SpawnConfig{
		Cmd: []string{"sh", "-c", "printf ok"},
		Env: map[string]string{},
	})
	if err != nil {
		t.Fatalf("spawn failed with empty env: %v", err)
	}

	stdout, stderr, result := readProcessOutput(t, handle)
	if result.Err != nil {
		t.Fatalf("wait failed: %v (stderr=%q)", result.Err, stderr)
	}
	if result.Code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%q)", result.Code, stderr)
	}
	if stdout != "ok" {
		t.Fatalf("expected output %q, got %q", "ok", stdout)
	}
}

// TestLocalRuntime_ExtraEnvMergedOntoParent verifies that extra env vars
// are added on top of the inherited parent environment, not replacing it.
func TestLocalRuntime_ExtraEnvMergedOntoParent(t *testing.T) {
	t.Setenv("PERSIST_AUTH_TOKEN", "daemon-token")

	rt := NewLocalRuntime()
	handle, err := rt.Spawn(testContext(t), localHelperConfig("print-env", map[string]string{
		"VISIBLE_VAR": "kept-local",
	}))
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	stdout, stderr, result := readProcessOutput(t, handle)
	if result.Err != nil {
		t.Fatalf("wait failed: %v (stderr=%q)", result.Err, stderr)
	}
	if result.Code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%q)", result.Code, stderr)
	}
	// Local runtime inherits parent env AND adds extra vars.
	if !strings.Contains(stdout, "VISIBLE_VAR=kept-local") {
		t.Fatalf("expected explicit env to remain visible, got %q", stdout)
	}
	// Parent env should also be present (inherited).
	if !strings.Contains(stdout, "HOME=") {
		t.Fatalf("expected HOME to be inherited from parent, got %q", stdout)
	}
}

func TestDockerRuntime_DoesNotInheritDaemonSecrets(t *testing.T) {
	t.Setenv("SECRET_KEY", "daemon-secret")
	contents, _ := dockerPreparedEnvFileContents(t, SpawnConfig{
		Cmd:       []string{"sh", "-c", "printf %s \"${SECRET_KEY-}\""},
		SessionID: "docker-env-secret",
		TaskID:    "docker-env-secret",
	})

	if strings.Contains(contents, "SECRET_KEY=") {
		t.Fatalf("expected SECRET_KEY to be absent from env file, got %q", contents)
	}
	if !strings.Contains(contents, "AGENT_CMD=[\"sh\"]\n") {
		t.Fatalf("expected binary-only AGENT_CMD in env file, got %q", contents)
	}
}

func TestDockerRuntime_ExplicitEnvIsAvailable(t *testing.T) {
	contents, _ := dockerPreparedEnvFileContents(t, SpawnConfig{
		Cmd:       []string{"sh", "-c", "printf %s \"$VISIBLE_VAR\""},
		Env:       map[string]string{"VISIBLE_VAR": "hello-docker"},
		SessionID: "docker-env-visible",
		TaskID:    "docker-env-visible",
	})

	if !strings.Contains(contents, "VISIBLE_VAR=hello-docker\n") {
		t.Fatalf("expected explicit env value in env file, got %q", contents)
	}
}

func TestDockerRuntime_RejectsReservedEnvOverrides(t *testing.T) {
	rt := dockerRuntimeForEnvTests()
	_, err := rt.prepareRun(SpawnConfig{
		Cmd: []string{"echo", "ignored"},
		Env: map[string]string{"PATH": "/tmp/malicious"},
	})
	if err == nil {
		t.Fatal("expected PATH override to be rejected")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved env error, got %v", err)
	}
}

func TestDockerRuntime_PassesMetacharactersLiterally(t *testing.T) {
	want := "`uname` $(whoami) ${HOME} && echo literal"
	contents, args := dockerPreparedEnvFileContents(t, SpawnConfig{
		Cmd:       []string{"sh", "-c", "printf %s \"$TEST_VALUE\""},
		Env:       map[string]string{"TEST_VALUE": want},
		SessionID: "docker-env-literal",
		TaskID:    "docker-env-literal",
	})

	if !strings.Contains(contents, "TEST_VALUE="+want+"\n") {
		t.Fatalf("expected literal TEST_VALUE in env file, got %q", contents)
	}
	if containsArg(args, "--privileged") {
		t.Fatalf("unexpected injected docker flag in args: %v", args)
	}
}

func TestDockerRuntime_InvalidEnvKeysError(t *testing.T) {
	rt := dockerRuntimeForEnvTests()

	cases := []struct {
		name string
		key  string
	}{
		{name: "equals", key: "BAD=KEY"},
		{name: "space", key: "BAD KEY"},
		{name: "newline", key: "BAD\nKEY"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.prepareRun(SpawnConfig{
				Cmd: []string{"echo", "ignored"},
				Env: map[string]string{tc.key: "value"},
			})
			if err == nil {
				t.Fatalf("expected invalid env key %q to fail", tc.key)
			}
			if !strings.Contains(err.Error(), "invalid env key") {
				t.Fatalf("expected invalid env key error, got %v", err)
			}
		})
	}
}

func TestDockerRuntime_EmptyEnvMapDoesNotPanic(t *testing.T) {
	contents, _ := dockerPreparedEnvFileContents(t, SpawnConfig{
		Cmd:       []string{"sh", "-c", "printf ok"},
		Env:       map[string]string{},
		SessionID: "docker-env-empty",
		TaskID:    "docker-env-empty",
	})

	if !strings.Contains(contents, "AGENT_CMD=[\"sh\"]\n") {
		t.Fatalf("expected AGENT_CMD in env file, got %q", contents)
	}
	if strings.Contains(contents, "HOME=") {
		t.Fatalf("expected empty env map to keep docker clean-room env, got %q", contents)
	}
}

func TestDockerRuntime_BuildRunArgsKeepEnvFlagsAtomic(t *testing.T) {
	rt := dockerRuntimeForEnvTests()
	spec, err := rt.prepareRun(SpawnConfig{
		Cmd: []string{"echo", "ok"},
		Env: map[string]string{
			"ALSO_SAFE": "value with spaces --privileged",
			"SAFE":      "$(touch /tmp/pwned)",
		},
		TaskID: "docker-env-args",
	})
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	defer spec.cleanup()

	args := spec.args
	envFile := flagValue(args, "--env-file")
	if envFile == "" {
		t.Fatalf("expected --env-file arg, got %v", args)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}

	if !strings.Contains(string(data), "ALSO_SAFE=value with spaces --privileged\n") {
		t.Fatalf("expected ALSO_SAFE in env file, got %q", string(data))
	}
	if !strings.Contains(string(data), "SAFE=$(touch /tmp/pwned)\n") {
		t.Fatalf("expected SAFE in env file, got %q", string(data))
	}
	if !strings.Contains(string(data), "HTTP_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected HTTP_PROXY in env file, got %q", string(data))
	}
	if !strings.Contains(string(data), "HTTPS_PROXY=http://agentruntime-proxy:3128\n") {
		t.Fatalf("expected HTTPS_PROXY in env file, got %q", string(data))
	}
	if !strings.Contains(string(data), "NO_PROXY=localhost,127.0.0.1,host.docker.internal\n") {
		t.Fatalf("expected NO_PROXY in env file, got %q", string(data))
	}
	if containsArg(args, "--privileged") {
		t.Fatalf("unexpected injected docker flag in args: %v", args)
	}
}

func TestDockerRuntime_FullEnvExcludesDaemonInternalVars(t *testing.T) {
	t.Setenv("PERSIST_AUTH_TOKEN", "daemon-token")
	t.Setenv("SECRET_KEY", "daemon-secret")

	contents, _ := dockerPreparedEnvFileContents(t, SpawnConfig{
		Cmd:       []string{"env"},
		Env:       map[string]string{"VISIBLE_VAR": "kept-docker"},
		SessionID: "docker-env-full",
		TaskID:    "docker-env-full",
	})

	if strings.Contains(contents, "PERSIST_AUTH_TOKEN=") {
		t.Fatalf("expected PERSIST_AUTH_TOKEN to be absent from env file, got %q", contents)
	}
	if strings.Contains(contents, "SECRET_KEY=") {
		t.Fatalf("expected SECRET_KEY to be absent from env file, got %q", contents)
	}
	if !strings.Contains(contents, "VISIBLE_VAR=kept-docker\n") {
		t.Fatalf("expected explicit env to remain visible, got %q", contents)
	}
}

func dockerPreparedEnvFileContents(t *testing.T, cfg SpawnConfig) (string, []string) {
	t.Helper()

	rt := dockerRuntimeForEnvTests()
	spec, err := rt.prepareRun(cfg)
	if err != nil {
		t.Fatalf("prepareRun failed: %v", err)
	}
	t.Cleanup(spec.cleanup)

	envFile := flagValue(spec.args, "--env-file")
	if envFile == "" {
		t.Fatalf("expected --env-file arg, got %v", spec.args)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}

	return string(data), spec.args
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func localHelperConfig(action string, env map[string]string) SpawnConfig {
	mergedEnv := map[string]string{
		"GO_WANT_HELPER_PROCESS": "1",
		"HELPER_ACTION":          action,
	}
	for key, value := range env {
		mergedEnv[key] = value
	}

	return SpawnConfig{
		Cmd: []string{os.Args[0], "-test.run=^TestRuntimeEnvHelperProcess$"},
		Env: mergedEnv,
	}
}

type readResult struct {
	data string
	err  error
}

func readProcessOutput(t *testing.T, handle ProcessHandle) (string, string, ExitResult) {
	t.Helper()

	stdoutCh := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(handle.Stdout())
		stdoutCh <- readResult{data: string(data), err: err}
	}()

	stderrCh := make(chan readResult, 1)
	if stderr := handle.Stderr(); stderr != nil {
		go func() {
			data, err := io.ReadAll(stderr)
			stderrCh <- readResult{data: string(data), err: err}
		}()
	} else {
		stderrCh <- readResult{}
	}

	result := <-handle.Wait()
	stdout := <-stdoutCh
	stderr := <-stderrCh

	if stdout.err != nil {
		t.Fatalf("read stdout failed: %v", stdout.err)
	}
	if stderr.err != nil {
		t.Fatalf("read stderr failed: %v", stderr.err)
	}

	return stdout.data, stderr.data, result
}

func requireDocker(t *testing.T) {
	t.Helper()
	if !dockerAvailable() {
		t.Skip("Docker not available")
	}
}

func dockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	return cmd.Run() == nil
}

func dockerRuntimeForEnvTests() *DockerRuntime {
	return NewDockerRuntime(DockerConfig{Image: "alpine:latest"})
}

func hasFlagValue(args []string, flag string, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}
