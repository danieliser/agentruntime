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

type Event struct {
	Type      string         `json:"type"`
	Data      map[string]any `json:"data,omitempty"`
	Offset    int64          `json:"offset,omitempty"`
	Timestamp int64          `json:"timestamp,omitempty"`
}

type Command struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

type sidecarHealthResponse struct {
	Status       string `json:"status"`
	AgentRunning bool   `json:"agent_running"`
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id"`
}

func TestE2E_Sidecar_Health(t *testing.T) {
	ctx := sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, "/bin/echo")
	defer cleanup()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
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

func TestE2E_Sidecar_ClaudeStructuredOutput(t *testing.T) {
	if !agentAvailable("claude") {
		t.Skip("claude not in PATH")
	}
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, "claude")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, "prompt", map[string]any{
		"content": "Reply with exactly SIDECAR_CLAUDE_OK and no other text.",
	})

	events := collectEvents(t, conn, 45*time.Second)
	assertHasAgentMessageWithText(t, events, "SIDECAR_CLAUDE_OK")
	assertHasAgentMessageUsage(t, events)
	assertHasEventType(t, events, "result")
}

func TestE2E_Sidecar_CodexStructuredOutput(t *testing.T) {
	if !agentAvailable("codex") {
		t.Skip("codex not in PATH")
	}
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, "codex")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, "prompt", map[string]any{
		"content": "Reply with exactly SIDECAR_CODEX_OK and no other text.",
	})

	events := collectEvents(t, conn, 45*time.Second)
	assertHasAgentMessageWithText(t, events, "SIDECAR_CODEX_OK")
	assertHasEventType(t, events, "result")
}

func TestE2E_Sidecar_CodexToolCall(t *testing.T) {
	if !agentAvailable("codex") {
		t.Skip("codex not in PATH")
	}
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, "codex")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, "prompt", map[string]any{
		"content": "Use a tool to run `printf SIDECAR_TOOL_OK` and then tell me the result.",
	})

	events := collectEvents(t, conn, 45*time.Second)
	assertHasEventType(t, events, "tool_use")
	assertHasEventType(t, events, "tool_result")
}

func TestE2E_Sidecar_ClaudeInterrupt(t *testing.T) {
	if !agentAvailable("claude") {
		t.Skip("claude not in PATH")
	}
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, "claude")
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, "prompt", map[string]any{
		"content": "Count from 1 to 400 slowly, one number per line, and do not summarize.",
	})

	initial, ok := waitForEventType(t, conn, 25*time.Second, "agent_message")
	if !ok {
		t.Fatal("timed out waiting for initial Claude response")
	}
	if text := eventText(initial); strings.Contains(text, "SIDECAR_INTERRUPT_OK") {
		t.Fatalf("unexpected interrupt marker in initial response: %q", text)
	}

	sendCommand(t, conn, "interrupt", nil)
	sendCommand(t, conn, "prompt", map[string]any{
		"content": "Reply with exactly SIDECAR_INTERRUPT_OK and no other text.",
	})

	deadline := time.Now().Add(30 * time.Second)
	var followup []Event
	for time.Now().Before(deadline) {
		batch := collectEvents(t, conn, time.Until(deadline))
		followup = append(followup, batch...)
		if hasAgentMessageWithText(followup, "SIDECAR_INTERRUPT_OK") {
			assertHasEventType(t, followup, "result")
			return
		}
		if hasEventType(followup, "exit") {
			break
		}
	}

	t.Fatalf("expected interrupted Claude response to change course, got events: %#v", followup)
}

func TestE2E_Sidecar_ReplayOnReconnect(t *testing.T) {
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, fakeClaudeBinary(t))
	defer cleanup()

	conn := dialSidecarWS(t, port)

	sendCommand(t, conn, "prompt", map[string]any{
		"content": "replay marker",
	})

	firstPass := collectEvents(t, conn, 10*time.Second)
	if err := conn.Close(); err != nil {
		t.Fatalf("close initial websocket: %v", err)
	}

	assertHasAgentMessageWithText(t, firstPass, "replay marker")
	assertHasEventType(t, firstPass, "result")

	replayConn := dialSidecarWSPath(t, port, "/ws?since=0")
	defer replayConn.Close()

	replayed := collectEvents(t, replayConn, 5*time.Second)
	assertHasAgentMessageWithText(t, replayed, "replay marker")
	assertHasEventType(t, replayed, "result")
}

func TestE2E_Sidecar_MultipleClients(t *testing.T) {
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, fakeClaudeBinary(t))
	defer cleanup()

	connA := dialSidecarWS(t, port)
	defer connA.Close()

	connB := dialSidecarWS(t, port)
	defer connB.Close()

	sendCommand(t, connA, "prompt", map[string]any{
		"content": "broadcast marker",
	})

	type eventResult struct {
		events []Event
		err    error
	}

	results := make(chan eventResult, 2)
	go func() {
		events, err := collectEventsWithError(connA, 10*time.Second)
		results <- eventResult{events: events, err: err}
	}()
	go func() {
		events, err := collectEventsWithError(connB, 10*time.Second)
		results <- eventResult{events: events, err: err}
	}()

	first := <-results
	second := <-results
	if first.err != nil {
		t.Fatalf("collect events from first client: %v", first.err)
	}
	if second.err != nil {
		t.Fatalf("collect events from second client: %v", second.err)
	}

	assertHasAgentMessageWithText(t, first.events, "broadcast marker")
	assertHasEventType(t, first.events, "result")
	assertHasAgentMessageWithText(t, second.events, "broadcast marker")
	assertHasEventType(t, second.events, "result")
}

func TestE2E_Sidecar_PromptCommand(t *testing.T) {
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, fakeClaudeBinary(t))
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, "prompt", map[string]any{
		"content": "prompt passthrough marker",
	})

	events := collectEvents(t, conn, 10*time.Second)
	assertHasAgentMessageWithText(t, events, "prompt passthrough marker")
	assertHasEventType(t, events, "result")
}

func TestE2E_Sidecar_UnknownCommand(t *testing.T) {
	sidecarTestContext(t)

	port, cleanup := startSidecarProcess(t, fakeClaudeBinary(t))
	defer cleanup()

	conn := dialSidecarWS(t, port)
	defer conn.Close()

	sendCommand(t, conn, "totally_unknown", map[string]any{
		"content": "ignored",
	})

	events := collectEvents(t, conn, 2*time.Second)
	assertHasErrorContaining(t, events, "unknown command type")
}

func startSidecarProcess(t *testing.T, agentCmd string) (int, func()) {
	t.Helper()

	cmdArgs := strings.Fields(strings.TrimSpace(agentCmd))
	if len(cmdArgs) == 0 {
		t.Fatal("agentCmd is required")
	}

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
			t.Logf("timed out waiting for sidecar to exit")
		}

		if t.Failed() && logs.Len() > 0 {
			t.Logf("sidecar logs:\n%s", logs.String())
		}
	}

	return port, cleanup
}

func dialSidecarWS(t *testing.T, port int) *websocket.Conn {
	t.Helper()
	return dialSidecarWSPath(t, port, "/ws")
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

func sendCommand(t *testing.T, conn *websocket.Conn, cmdType string, data map[string]any) {
	t.Helper()

	if err := conn.WriteJSON(Command{
		Type: cmdType,
		Data: data,
	}); err != nil {
		t.Fatalf("write command %q: %v", cmdType, err)
	}
}

func collectEvents(t *testing.T, conn *websocket.Conn, timeout time.Duration) []Event {
	t.Helper()

	events, err := collectEventsWithError(conn, timeout)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}

func collectEventsWithError(conn *websocket.Conn, timeout time.Duration) ([]Event, error) {
	deadline := time.Now().Add(timeout)
	var events []Event

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return events, nil
		}

		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return nil, err
		}

		var event Event
		if err := conn.ReadJSON(&event); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return events, nil
			}
			return nil, err
		}

		events = append(events, event)
		if event.Type == "result" || event.Type == "exit" {
			return events, nil
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

func fakeClaudeBinary(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude")
	writeExecutable(t, path, fakeClaudeSidecarScript)
	return path
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

func sidecarTestContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), perTestTimeout)
	t.Cleanup(cancel)
	return ctx
}

func waitForEventType(t *testing.T, conn *websocket.Conn, timeout time.Duration, wantType string) (Event, bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}

		var event Event
		if err := conn.ReadJSON(&event); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return Event{}, false
			}
			t.Fatalf("read event: %v", err)
		}

		if event.Type == wantType {
			return event, true
		}
	}

	return Event{}, false
}

func assertHasEventType(t *testing.T, events []Event, want string) {
	t.Helper()

	if !hasEventType(events, want) {
		t.Fatalf("expected event type %q in %#v", want, events)
	}
}

func assertHasAgentMessageWithText(t *testing.T, events []Event, needle string) {
	t.Helper()

	if !hasAgentMessageWithText(events, needle) {
		t.Fatalf("expected agent_message containing %q in %#v", needle, events)
	}
}

func assertHasAgentMessageUsage(t *testing.T, events []Event) {
	t.Helper()

	for _, event := range events {
		if event.Type != "agent_message" {
			continue
		}
		if usage, ok := event.Data["usage"].(map[string]any); ok && len(usage) > 0 {
			return
		}
	}

	t.Fatalf("expected agent_message usage in %#v", events)
}

func assertHasErrorContaining(t *testing.T, events []Event, needle string) {
	t.Helper()

	for _, event := range events {
		if event.Type != "error" {
			continue
		}
		if strings.Contains(eventText(event), needle) {
			return
		}
	}

	t.Fatalf("expected error containing %q in %#v", needle, events)
}

func hasEventType(events []Event, want string) bool {
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}

func hasAgentMessageWithText(events []Event, needle string) bool {
	for _, event := range events {
		if event.Type != "agent_message" {
			continue
		}
		if strings.Contains(eventText(event), needle) {
			return true
		}
	}
	return false
}

func eventText(event Event) string {
	if event.Data == nil {
		return ""
	}
	for _, key := range []string{"text", "message"} {
		if value, ok := event.Data[key].(string); ok {
			return value
		}
	}
	return ""
}

const fakeClaudeSidecarScript = `#!/bin/sh

session_id=""

while [ $# -gt 0 ]; do
	case "$1" in
		--session-id)
			session_id="$2"
			shift 2
			;;
		*)
			shift
			;;
	esac
done

json_escape() {
	printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

extract_prompt() {
	printf '%s\n' "$1" | sed -n 's/.*"text":"\([^"]*\)".*/\1/p'
}

while IFS= read -r line; do
	case "$line" in
		*"\"type\":\"control_request\""*)
			continue
			;;
	esac

	prompt=$(extract_prompt "$line")
	if [ -z "$prompt" ]; then
		continue
	fi

	escaped=$(json_escape "$prompt")

	if printf '%s' "$prompt" | grep -qi 'tool'; then
		printf '%s\n' "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"$escaped\"},{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"Bash\",\"input\":{\"cmd\":\"printf SIDE_CAR_TOOL\"}}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"
	else
		printf '%s\n' "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"$escaped\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"
	fi

	printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"$session_id\",\"duration_ms\":5,\"num_turns\":1}"
done
`
