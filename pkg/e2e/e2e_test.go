//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/api"
	"github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/client"
)

const (
	perTestTimeout = 60 * time.Second
	liveE2EEnv     = "AGENTRUNTIME_RUN_LIVE_E2E"
)

var (
	agentdBuildOnce sync.Once
	agentdBinary    string
	agentdBuildErr  error
)

type daemonOptions struct {
	useFakeClaude bool
}

type daemonEnv struct {
	baseURL string
	client  *client.Client
	cmd     *exec.Cmd
	logs    *bytes.Buffer
	dataDir string
	waitCh  chan error
}

func TestE2E_HealthCheck(t *testing.T) {
	runWithDaemon(t, daemonOptions{}, func(ctx context.Context, env *daemonEnv) {
		health, err := env.client.Health(ctx)
		if err != nil {
			t.Fatalf("health request failed: %v", err)
		}
		if health.Status != "ok" {
			t.Fatalf("expected health status ok, got %q", health.Status)
		}
		if health.Runtime != "local" {
			t.Fatalf("expected runtime local, got %q", health.Runtime)
		}
	})
}

func TestE2E_CreateSession_Echo(t *testing.T) {
	runWithDaemon(t, daemonOptions{useFakeClaude: true}, func(ctx context.Context, env *daemonEnv) {
		resp, err := env.client.Dispatch(ctx, api.SessionRequest{
			Agent:   "claude",
			Prompt:  "hello",
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}
		if resp.SessionID == "" {
			t.Fatal("expected non-empty session id")
		}

		frames := collectFramesUntilExit(t, ctx, resp.WSURL)
		if !hasStdoutSubstring(frames, `"result":"hello"`) {
			t.Fatalf("expected stdout frame containing echoed result, got %#v", frames)
		}
		exitCode, ok := exitCodeFromFrames(frames)
		if !ok {
			t.Fatalf("expected exit frame, got %#v", frames)
		}
		if exitCode != 0 {
			t.Fatalf("expected exit code 0, got %d", exitCode)
		}

		waitForSessionStatus(t, ctx, env.client, resp.SessionID, "completed")

		logData, err := env.client.GetLog(ctx, resp.SessionID)
		if err != nil {
			t.Fatalf("get log failed: %v", err)
		}
		assertNDJSON(t, logData)
	})
}

func TestE2E_ListSessions(t *testing.T) {
	runWithDaemon(t, daemonOptions{useFakeClaude: true}, func(ctx context.Context, env *daemonEnv) {
		for i := 0; i < 3; i++ {
			resp, err := env.client.Dispatch(ctx, api.SessionRequest{
				Agent:   "claude",
				Prompt:  fmt.Sprintf("list-%d", i),
				WorkDir: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("dispatch %d failed: %v", i, err)
			}
			if resp.SessionID == "" {
				t.Fatalf("dispatch %d returned empty session id", i)
			}
		}

		sessions, err := env.client.ListSessions(ctx)
		if err != nil {
			t.Fatalf("list sessions failed: %v", err)
		}
		if len(sessions) != 3 {
			t.Fatalf("expected 3 sessions, got %d", len(sessions))
		}
	})
}

func TestE2E_KillSession(t *testing.T) {
	runWithDaemon(t, daemonOptions{useFakeClaude: true}, func(ctx context.Context, env *daemonEnv) {
		resp, err := env.client.Dispatch(ctx, api.SessionRequest{
			Agent:   "claude",
			Prompt:  "__sleep__",
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		if err := env.client.Kill(ctx, resp.SessionID); err != nil {
			t.Fatalf("kill failed: %v", err)
		}

		waitForSessionStatus(t, ctx, env.client, resp.SessionID, "failed")
	})
}

func TestE2E_LogPolling(t *testing.T) {
	runWithDaemon(t, daemonOptions{useFakeClaude: true}, func(ctx context.Context, env *daemonEnv) {
		resp, err := env.client.Dispatch(ctx, api.SessionRequest{
			Agent:   "claude",
			Prompt:  "polling",
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		var (
			data       []byte
			nextCursor int64
		)
		waitUntil(t, ctx, "log cursor to advance", func() (bool, error) {
			data, nextCursor, err = env.client.GetLogs(ctx, resp.SessionID, 0)
			if err != nil {
				return false, err
			}
			return len(data) > 0 && nextCursor > 0, nil
		})

		if !strings.Contains(string(data), `"result":"polling"`) {
			t.Fatalf("expected buffered output, got %q", string(data))
		}
		if nextCursor <= 0 {
			t.Fatalf("expected cursor > 0, got %d", nextCursor)
		}
	})
}

func TestE2E_SessionInfo(t *testing.T) {
	runWithDaemon(t, daemonOptions{useFakeClaude: true}, func(ctx context.Context, env *daemonEnv) {
		resp, err := env.client.Dispatch(ctx, api.SessionRequest{
			Agent:   "claude",
			Prompt:  "info",
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		var info *api.SessionInfo
		waitUntil(t, ctx, "session info files to exist", func() (bool, error) {
			info, err = env.client.GetSessionInfo(ctx, resp.SessionID)
			if err != nil {
				return false, err
			}
			if info.SessionDir == "" || info.LogFile == "" {
				return false, nil
			}
			if _, err := os.Stat(info.SessionDir); err != nil {
				return false, nil
			}
			if _, err := os.Stat(info.LogFile); err != nil {
				return false, nil
			}
			return true, nil
		})

		if info.SessionDir == "" {
			t.Fatal("expected non-empty session_dir")
		}
		if info.LogFile == "" {
			t.Fatal("expected non-empty log_file")
		}
	})
}

func TestE2E_ReplayOnReconnect(t *testing.T) {
	runWithDaemon(t, daemonOptions{useFakeClaude: true}, func(ctx context.Context, env *daemonEnv) {
		resp, err := env.client.Dispatch(ctx, api.SessionRequest{
			Agent:   "claude",
			Prompt:  "replay-check",
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		waitUntil(t, ctx, "session output to appear", func() (bool, error) {
			data, _, err := env.client.GetLogs(ctx, resp.SessionID, 0)
			if err != nil {
				return false, err
			}
			return strings.Contains(string(data), `"result":"replay-check"`), nil
		})

		conn := dialWS(t, ctx, resp.WSURL+"?since=0")
		defer conn.Close()

		frame := readFrame(t, conn)
		if frame.Type != "replay" {
			t.Fatalf("expected replay frame, got %#v", frame)
		}

		replayed, err := base64.StdEncoding.DecodeString(frame.Data)
		if err != nil {
			t.Fatalf("decode replay frame: %v", err)
		}
		if !strings.Contains(string(replayed), `"result":"replay-check"`) {
			t.Fatalf("expected replay payload to contain session output, got %q", string(replayed))
		}
	})
}

func TestE2E_Claude_OAuth(t *testing.T) {
	requireLiveAgent(t, "claude")

	runWithDaemon(t, daemonOptions{}, func(ctx context.Context, env *daemonEnv) {
		resp, err := env.client.Dispatch(ctx, api.SessionRequest{
			Agent:   "claude",
			Prompt:  "Reply with exactly E2E_CLAUDE_OK",
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		frames := collectFramesUntilExit(t, ctx, resp.WSURL)
		if !hasStdoutSubstring(frames, "E2E_CLAUDE_OK") {
			t.Fatalf("expected Claude output to contain marker, got %#v", frames)
		}
	})
}

func TestE2E_Codex_OAuth(t *testing.T) {
	requireLiveAgent(t, "codex")

	runWithDaemon(t, daemonOptions{}, func(ctx context.Context, env *daemonEnv) {
		resp, err := env.client.Dispatch(ctx, api.SessionRequest{
			Agent:   "codex",
			Prompt:  "Reply with exactly E2E_CODEX_OK",
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		frames := collectFramesUntilExit(t, ctx, resp.WSURL)
		if !hasStdoutSubstring(frames, "E2E_CODEX_OK") {
			t.Fatalf("expected Codex output to contain marker, got %#v", frames)
		}
	})
}

func runWithDaemon(t *testing.T, opts daemonOptions, fn func(context.Context, *daemonEnv)) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), perTestTimeout)
	t.Cleanup(cancel)

	env := startDaemon(t, ctx, opts)
	fn(ctx, env)
}

func startDaemon(t *testing.T, ctx context.Context, opts daemonOptions) *daemonEnv {
	t.Helper()

	binary := buildAgentdBinary(t)
	port := freePort(t)
	dataDir := t.TempDir()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.CommandContext(ctx, binary, "--port", strconv.Itoa(port), "--runtime", "local", "--data-dir", dataDir)
	cmd.Dir = repoRoot(t)

	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	cmd.Env = daemonEnvVars(t, opts)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start agentd: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	env := &daemonEnv{
		baseURL: baseURL,
		client:  client.New(baseURL),
		cmd:     cmd,
		logs:    &logs,
		dataDir: dataDir,
		waitCh:  waitCh,
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}

		select {
		case err := <-waitCh:
			if err != nil && !isExpectedProcessExit(err) {
				t.Logf("agentd exit error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Logf("timed out waiting for agentd to exit")
		}

		if t.Failed() && logs.Len() > 0 {
			t.Logf("agentd logs:\n%s", logs.String())
		}
	})

	waitUntil(t, ctx, "daemon health check", func() (bool, error) {
		health, err := env.client.Health(ctx)
		if err != nil {
			return false, nil
		}
		return health.Status == "ok", nil
	})

	return env
}

func buildAgentdBinary(t *testing.T) string {
	t.Helper()

	agentdBuildOnce.Do(func() {
		outDir, err := os.MkdirTemp("", "agentruntime-e2e-bin-")
		if err != nil {
			agentdBuildErr = err
			return
		}

		agentdBinary = filepath.Join(outDir, "agentd")
		buildCmd := exec.Command("go", "build", "-o", agentdBinary, "./cmd/agentd")
		buildCmd.Dir = repoRoot(t)

		var output bytes.Buffer
		buildCmd.Stdout = &output
		buildCmd.Stderr = &output
		if err := buildCmd.Run(); err != nil {
			agentdBuildErr = fmt.Errorf("go build agentd: %w\n%s", err, output.String())
		}
	})

	if agentdBuildErr != nil {
		t.Fatalf("build agentd binary: %v", agentdBuildErr)
	}
	return agentdBinary
}

func daemonEnvVars(t *testing.T, opts daemonOptions) []string {
	t.Helper()

	env := os.Environ()
	if !opts.useFakeClaude {
		return env
	}

	fakeBinDir := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBinDir, "claude"), fakeClaudeScript)

	pathValue := fakeBinDir
	if currentPath := os.Getenv("PATH"); currentPath != "" {
		pathValue += string(os.PathListSeparator) + currentPath
	}
	return append(env, "PATH="+pathValue)
}

func collectFramesUntilExit(t *testing.T, ctx context.Context, wsURL string) []bridge.ServerFrame {
	t.Helper()

	conn := dialWS(t, ctx, wsURL)
	defer conn.Close()

	var frames []bridge.ServerFrame
	for {
		frame := readFrame(t, conn)
		frames = append(frames, frame)
		if frame.Type == "exit" {
			return frames
		}
	}
}

func dialWS(t *testing.T, ctx context.Context, wsURL string) *websocket.Conn {
	t.Helper()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dial websocket %s: %v %s", wsURL, err, status)
	}
	return conn
}

func readFrame(t *testing.T, conn *websocket.Conn) bridge.ServerFrame {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))

	var frame bridge.ServerFrame
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("read websocket frame: %v", err)
	}
	return frame
}

func waitForSessionStatus(t *testing.T, ctx context.Context, cl *client.Client, sessionID string, want string) {
	t.Helper()

	waitUntil(t, ctx, "session status "+want, func() (bool, error) {
		sess, err := cl.GetSession(ctx, sessionID)
		if err != nil {
			return false, err
		}
		return sess.Status == want, nil
	})
}

func waitUntil(t *testing.T, ctx context.Context, label string, check func() (bool, error)) {
	t.Helper()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		ok, err := check()
		if err != nil {
			t.Fatalf("wait for %s: %v", label, err)
		}
		if ok {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", label, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertNDJSON(t *testing.T, data []byte) {
	t.Helper()

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		t.Fatal("expected non-empty NDJSON payload")
	}

	for _, line := range strings.Split(trimmed, "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("invalid NDJSON line: %q", line)
		}
	}
}

func hasStdoutSubstring(frames []bridge.ServerFrame, needle string) bool {
	for _, frame := range frames {
		if frame.Type == "stdout" && strings.Contains(frame.Data, needle) {
			return true
		}
	}
	return false
}

func exitCodeFromFrames(frames []bridge.ServerFrame) (int, bool) {
	for _, frame := range frames {
		if frame.Type == "exit" && frame.ExitCode != nil {
			return *frame.ExitCode, true
		}
	}
	return 0, false
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer ln.Close()

	return ln.Addr().(*net.TCPAddr).Port
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := stdruntime.Caller(0)
	if !ok {
		t.Fatal("determine caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func requireLiveAgent(t *testing.T, name string) {
	t.Helper()

	if !agentAvailable(name) {
		t.Skipf("%s not in PATH", name)
	}
	if os.Getenv(liveE2EEnv) != "1" {
		t.Skipf("live E2E disabled; set %s=1 to enable", liveE2EEnv)
	}
}

func agentAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func isExpectedProcessExit(err error) bool {
	if err == nil {
		return true
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}

	if exitErr.ExitCode() >= 0 {
		return true
	}

	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok {
		return waitStatus.Signaled()
	}

	return false
}

// fakeClaudeScript lets the fast E2E tests exercise the stock Claude agent path
// without hitting external services. Prompt mode emits a single NDJSON result
// line, and interactive mode falls back to stdin passthrough.
var fakeClaudeScript = strings.TrimSpace(`
#!/bin/sh

prompt=""
interactive=1

while [ $# -gt 0 ]; do
	case "$1" in
		-p)
			prompt="$2"
			interactive=0
			shift 2
			;;
		--output-format|--model|--max-turns|--session-id|--allowedTools)
			shift 2
			;;
		--resume|--verbose)
			shift
			;;
		*)
			shift
			;;
	esac
done

if [ "$interactive" -eq 0 ]; then
	if [ "$prompt" = "__sleep__" ]; then
		sleep 60
		exit 0
	fi

	printf '%s\n' "{\"type\":\"result\",\"result\":\"$prompt\",\"subtype\":\"success\"}"
	sleep 1
	exit 0
fi

cat
`) + "\n"

func init() {
	// websocket URLs are returned directly by the API; keep them normalized in tests.
	websocket.DefaultDialer.EnableCompression = false
}
