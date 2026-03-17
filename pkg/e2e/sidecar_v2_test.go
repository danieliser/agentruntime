//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var (
	sidecarBuildOnce sync.Once
	sidecarBinary    string
	sidecarBuildErr  error
)

type sidecarHealthResponse struct {
	Status       string `json:"status"`
	AgentRunning bool   `json:"agent_running"`
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id"`
}

func TestE2E_V2_Health(t *testing.T) {
	t.Helper()

	port, cleanup := startSidecarLocal(t, `["echo","hello"]`, "")
	defer cleanup()

	req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		t.Fatalf("create health request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var health sidecarHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if health.Status != "ok" {
		t.Fatalf("health status payload = %q, want ok", health.Status)
	}
	if health.AgentType != "echo" {
		t.Fatalf("health agent_type = %q, want echo", health.AgentType)
	}
	if health.AgentRunning {
		t.Fatal("expected agent_running false before websocket connect")
	}
}

func TestE2E_V2_EchoAgent(t *testing.T) {
	port, cleanup := startSidecarLocal(t, `["echo","hello"]`, "")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	events := readEvents(t, conn, 3*time.Second)
	assertEventText(t, events, "stdout", "hello")
	assertEventType(t, events, "exit")
}

func TestE2E_V2_Replay(t *testing.T) {
	port, cleanup := startSidecarLocal(t, commandSpec(t, writeMockAgent(t, mockPromptEchoScript)), "")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	sendCommand(t, conn, map[string]any{
		"type": "prompt",
		"data": map[string]any{"content": "replay marker"},
	})

	firstPass := readEvents(t, conn, 1500*time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatalf("close initial websocket: %v", err)
	}

	assertEventText(t, firstPass, "stdout", "replay marker")

	replayConn := dialSidecarWSPath(t, port, "/ws?since=0")
	defer replayConn.Close()

	replayed := readEvents(t, replayConn, 1500*time.Millisecond)
	assertEventText(t, replayed, "stdout", "replay marker")
}

func TestE2E_V2_MultipleClients(t *testing.T) {
	port, cleanup := startSidecarLocal(t, commandSpec(t, writeMockAgent(t, mockPromptEchoScript)), "")
	defer cleanup()

	connA := dialSidecarWS(t, port)
	defer connA.Close()
	connB := dialSidecarWS(t, port)
	defer connB.Close()

	sendCommand(t, connA, map[string]any{
		"type": "prompt",
		"data": map[string]any{"content": "broadcast marker"},
	})

	first := readEvents(t, connA, 1500*time.Millisecond)
	second := readEvents(t, connB, 1500*time.Millisecond)

	assertEventText(t, first, "stdout", "broadcast marker")
	assertEventText(t, second, "stdout", "broadcast marker")
}

func TestE2E_V2_PromptCommand(t *testing.T) {
	port, cleanup := startSidecarLocal(t, commandSpec(t, writeMockAgent(t, mockPromptEchoScript)), "")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, map[string]any{
		"type": "prompt",
		"data": map[string]any{"content": "prompt passthrough marker"},
	})

	events := readEvents(t, conn, 1500*time.Millisecond)
	assertEventText(t, events, "stdout", "prompt passthrough marker")
}

func TestE2E_V2_UnknownCommand(t *testing.T) {
	port, cleanup := startSidecarLocal(t, commandSpec(t, writeMockAgent(t, mockPromptEchoScript)), "")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, map[string]any{
		"type": "totally_unknown",
		"data": map[string]any{"content": "ignored"},
	})

	events := readEvents(t, conn, 1500*time.Millisecond)
	assertEventText(t, events, "error", "unknown command type")
}

func TestE2E_V2_ClaudePromptMode(t *testing.T) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not in PATH")
	}

	port, cleanup := startSidecarLocal(t, commandSpec(t, claudePath), "Reply with exactly SIDECAR_V2_CLAUDE_OK and no other text.")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	events := readEvents(t, conn, 45*time.Second)
	assertNormalizedAgentMessage(t, events, "SIDECAR_V2_CLAUDE_OK")
	assertNormalizedResult(t, events)
}

func TestE2E_V2_CodexPromptMode(t *testing.T) {
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not in PATH")
	}

	port, cleanup := startSidecarLocal(t, commandSpec(t, codexPath), "Reply with exactly SIDECAR_V2_CODEX_OK and no other text.")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	events := readEvents(t, conn, 45*time.Second)
	assertNormalizedAgentMessage(t, events, "SIDECAR_V2_CODEX_OK")
	assertNormalizedResult(t, events)
}

func startSidecarLocal(t *testing.T, agentCmd, prompt string) (int, func()) {
	t.Helper()

	cmdArgs := parseCommandSpec(t, agentCmd)
	encodedCmd, err := json.Marshal(cmdArgs)
	if err != nil {
		t.Fatalf("marshal AGENT_CMD: %v", err)
	}

	port := freePort(t)
	homeDir := t.TempDir()
	binary := buildSidecarBinary(t)

	cmd := exec.Command(binary)
	cmd.Dir = repoRoot(t)

	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	cmd.Env = append(os.Environ(),
		"AGENT_CMD="+string(encodedCmd),
		fmt.Sprintf("SIDECAR_PORT=%d", port),
		"HOME="+homeDir,
	)
	if prompt != "" {
		cmd.Env = append(cmd.Env, "AGENT_PROMPT="+prompt)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	waitForSidecarHealth(t, port)

	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}

		select {
		case err := <-waitCh:
			if err != nil && !isExpectedProcessExit(err) {
				t.Logf("sidecar exit error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Log("timed out waiting for sidecar to exit")
		}

		if t.Failed() && logs.Len() > 0 {
			t.Logf("sidecar logs:\n%s", logs.String())
		}
	}

	return port, cleanup
}

func dialSidecarWS(t *testing.T, port int) *websocket.Conn {
	t.Helper()
	return dialSidecarWSPath(t, port, "/ws?since=0")
}

func dialSidecarWSPath(t *testing.T, port int, path string) *websocket.Conn {
	t.Helper()

	url := fmt.Sprintf("ws://127.0.0.1:%d%s", port, path)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial(url, nil)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dial sidecar websocket %s: %v %s", url, err, status)
	}
	return conn
}

func sendCommand(t *testing.T, conn *websocket.Conn, cmd map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(cmd); err != nil {
		t.Fatalf("write command: %v", err)
	}
}

func readEvents(t *testing.T, conn *websocket.Conn, timeout time.Duration) []map[string]any {
	t.Helper()

	var events []map[string]any
	for {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}

		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return events
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return events
			}
			t.Fatalf("read event: %v", err)
		}

		events = append(events, event)
		if eventType(event) == "result" || eventType(event) == "exit" {
			return events
		}
	}
}

func buildSidecarBinary(t *testing.T) string {
	t.Helper()

	sidecarBuildOnce.Do(func() {
		outDir, err := os.MkdirTemp("", "agentruntime-sidecar-e2e-")
		if err != nil {
			sidecarBuildErr = err
			return
		}

		sidecarBinary = filepath.Join(outDir, "sidecar")
		buildCmd := exec.Command("go", "build", "-o", sidecarBinary, "./cmd/sidecar")
		buildCmd.Dir = repoRoot(t)

		var output bytes.Buffer
		buildCmd.Stdout = &output
		buildCmd.Stderr = &output
		if err := buildCmd.Run(); err != nil {
			sidecarBuildErr = fmt.Errorf("go build sidecar: %w\n%s", err, output.String())
		}
	})

	if sidecarBuildErr != nil {
		t.Fatalf("build sidecar binary: %v", sidecarBuildErr)
	}
	return sidecarBinary
}

func waitForSidecarHealth(t *testing.T, port int) {
	t.Helper()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for sidecar health on port %d", port)
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), perTestTimeout)
	t.Cleanup(cancel)
	return ctx
}

func parseCommandSpec(t *testing.T, spec string) []string {
	t.Helper()

	spec = strings.TrimSpace(spec)
	if spec == "" {
		t.Fatal("agentCmd is required")
	}

	var cmd []string
	if strings.HasPrefix(spec, "[") {
		if err := json.Unmarshal([]byte(spec), &cmd); err != nil {
			t.Fatalf("parse AGENT_CMD JSON: %v", err)
		}
	} else {
		cmd = strings.Fields(spec)
	}
	if len(cmd) == 0 || strings.TrimSpace(cmd[0]) == "" {
		t.Fatal("agentCmd must contain a command")
	}
	return cmd
}

func commandSpec(t *testing.T, binary string, args ...string) string {
	t.Helper()

	cmd := append([]string{binary}, args...)
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command spec: %v", err)
	}
	return string(data)
}

func writeMockAgent(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mock-agent")
	writeExecutable(t, path, content)
	return path
}

func assertEventType(t *testing.T, events []map[string]any, want string) {
	t.Helper()
	for _, event := range events {
		if eventType(event) == want {
			return
		}
	}
	t.Fatalf("expected event type %q in %#v", want, events)
}

func assertEventText(t *testing.T, events []map[string]any, wantType, needle string) {
	t.Helper()
	for _, event := range events {
		if eventType(event) != wantType {
			continue
		}
		if strings.Contains(eventText(event), needle) {
			return
		}
	}
	t.Fatalf("expected %s event containing %q in %#v", wantType, needle, events)
}

func assertNormalizedResult(t *testing.T, events []map[string]any) {
	t.Helper()

	for _, event := range events {
		if eventType(event) != "result" {
			continue
		}
		data, _ := event["data"].(map[string]any)
		if data == nil {
			continue
		}
		if _, ok := data["status"].(string); ok {
			return
		}
	}

	t.Fatalf("expected normalized result event with status in %#v", events)
}

func assertNormalizedAgentMessage(t *testing.T, events []map[string]any, needle string) {
	t.Helper()

	for _, event := range events {
		if eventType(event) != "agent_message" {
			continue
		}

		data, _ := event["data"].(map[string]any)
		if data == nil {
			continue
		}

		text, _ := data["text"].(string)
		_, hasDelta := data["delta"].(bool)
		if hasDelta && strings.Contains(text, needle) {
			return
		}
	}

	t.Fatalf("expected normalized agent_message containing %q in %#v", needle, events)
}

func eventType(event map[string]any) string {
	value, _ := event["type"].(string)
	return value
}

func eventText(event map[string]any) string {
	data, _ := event["data"].(map[string]any)
	if data == nil {
		return ""
	}
	for _, key := range []string{"text", "message"} {
		if value, ok := data[key].(string); ok {
			return value
		}
	}
	return ""
}

const mockPromptEchoScript = `#!/bin/sh
set -eu

while IFS= read -r line; do
	printf '%s\n' "$line"
done
`
