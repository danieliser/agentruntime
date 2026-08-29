package runtime

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestPrepareLocalCommand_AppendsScopedMCPWithoutReplacingHome(t *testing.T) {
	const token = "local-runtime-token"
	req := &apischema.SessionRequest{
		Agent: "codex",
		MCPServers: []apischema.MCPServer{{
			Name: "persist", Type: "http",
			URL: "http://${HOST_GATEWAY}:8803/api/v1/mcp", Token: token,
		}},
	}
	cfg := SpawnConfig{
		AgentName: "codex",
		Cmd:       []string{"codex", "app-server", "--listen", "stdio://", "--strict-config"},
		Request:   req,
	}
	command, err := prepareLocalCommand(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(command, cfg.Cmd) || !strings.Contains(strings.Join(command, " "), `mcp_servers.persist.url=`) {
		t.Fatalf("prepared command = %#v", command)
	}
	if strings.Contains(strings.Join(command, " "), token) {
		t.Fatal("plaintext MCP token reached process argv")
	}
	if _, found := req.Env["HOME"]; found {
		t.Fatal("local runtime replaced HOME and hid ambient AutoVault skills")
	}
	if got := requestEnv(cfg); len(got) != 1 {
		t.Fatalf("spawn env = %#v, want only generated token reference", got)
	}
}

func TestLocalRuntime_Name(t *testing.T) {
	rt := NewLocalRuntime()
	if rt.Name() != "local" {
		t.Fatalf("expected name 'local', got %q", rt.Name())
	}
}

func TestLocalRuntime_SpawnEcho(t *testing.T) {
	rt := NewLocalRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handle, err := rt.Spawn(ctx, SpawnConfig{
		Cmd: []string{"echo", "hello world"},
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	// Read stdout.
	out, err := io.ReadAll(handle.Stdout())
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	if !strings.Contains(string(out), "hello world") {
		t.Fatalf("expected 'hello world' in output, got %q", string(out))
	}

	// Wait for exit.
	result := <-handle.Wait()
	if result.Code != 0 {
		t.Fatalf("expected exit code 0, got %d", result.Code)
	}
}

func TestLocalRuntime_SpawnWithEnv(t *testing.T) {
	rt := NewLocalRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handle, err := rt.Spawn(ctx, SpawnConfig{
		Cmd: []string{"sh", "-c", "echo $TEST_VAR"},
		Env: map[string]string{"TEST_VAR": "hello_env"},
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	out, err := io.ReadAll(handle.Stdout())
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	if !strings.Contains(string(out), "hello_env") {
		t.Fatalf("expected 'hello_env' in output, got %q", string(out))
	}

	result := <-handle.Wait()
	if result.Code != 0 {
		t.Fatalf("expected exit code 0, got %d", result.Code)
	}
}

func TestLocalRuntime_SpawnNonZeroExit(t *testing.T) {
	rt := NewLocalRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handle, err := rt.Spawn(ctx, SpawnConfig{
		Cmd: []string{"sh", "-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	result := <-handle.Wait()
	if result.Code != 42 {
		t.Fatalf("expected exit code 42, got %d", result.Code)
	}
}

func TestLocalRuntime_SpawnEmptyCmd(t *testing.T) {
	rt := NewLocalRuntime()
	_, err := rt.Spawn(context.Background(), SpawnConfig{})
	if err == nil {
		t.Fatal("expected error for empty cmd")
	}
}

func TestLocalRuntime_Kill(t *testing.T) {
	rt := NewLocalRuntime()
	ctx := context.Background()

	handle, err := rt.Spawn(ctx, SpawnConfig{
		Cmd: []string{"sleep", "60"},
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	if err := handle.Kill(); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	select {
	case <-handle.Wait():
		// Process exited after kill — good.
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after kill")
	}
}

func TestLocalRuntime_PID(t *testing.T) {
	rt := NewLocalRuntime()
	ctx := context.Background()

	handle, err := rt.Spawn(ctx, SpawnConfig{
		Cmd: []string{"sleep", "1"},
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	defer handle.Kill()

	if handle.PID() <= 0 {
		t.Fatalf("expected positive PID, got %d", handle.PID())
	}
}

func TestLocalRuntime_RecoverEmpty(t *testing.T) {
	rt := NewLocalRuntime()
	handles, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover failed: %v", err)
	}
	if handles != nil {
		t.Fatalf("expected nil handles, got %v", handles)
	}
}

func TestLocalRuntime_StdinWrite(t *testing.T) {
	rt := NewLocalRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handle, err := rt.Spawn(ctx, SpawnConfig{
		Cmd: []string{"cat"},
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	// Write to stdin then close.
	_, err = handle.Stdin().Write([]byte("piped input\n"))
	if err != nil {
		t.Fatalf("stdin write failed: %v", err)
	}
	handle.Stdin().Close()

	// Read stdout.
	out, err := io.ReadAll(handle.Stdout())
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	if !strings.Contains(string(out), "piped input") {
		t.Fatalf("expected 'piped input' in output, got %q", string(out))
	}
}
