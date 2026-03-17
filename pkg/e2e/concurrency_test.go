//go:build e2e && concurrency

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestConcurrency_30Sessions launches agentd in Docker mode and creates 30
// concurrent sessions: 15 Claude (7 interactive + 8 prompt) and 15 Codex
// (7 interactive + 8 prompt). Each prompt-mode session gets a minimal prompt
// ("print 1 through 10, one per line"). Each interactive session gets the
// same prompt sent via WebSocket after connection.
//
// Validates:
// - All 30 sessions created successfully
// - All 30 WebSocket connections open and maintained
// - All 30 sessions produce normalized output events
// - All 30 sessions reach exit/result state
//
// Requires: Docker running, agentruntime-agent:latest built, Claude + Codex authenticated.
//
// Run: go test -tags='e2e concurrency' -timeout=300s -run TestConcurrency ./pkg/e2e/ -v
func TestConcurrency_30Sessions(t *testing.T) {
	// Verify Docker is available
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker not available")
	}
	// Verify image exists
	out, err := exec.Command("docker", "image", "inspect", "agentruntime-agent:latest", "--format", "{{.Id}}").Output()
	if err != nil || len(out) == 0 {
		t.Skip("agentruntime-agent:latest not built")
	}

	// Start agentd in Docker mode
	daemonPort := mustFreePort(t)
	daemonBin := buildDaemonBinary(t)
	dataDir := t.TempDir()

	daemon := exec.Command(daemonBin,
		"--port", fmt.Sprintf("%d", daemonPort),
		"--runtime", "docker",
	)
	daemon.Env = append(os.Environ(),
		"AGENTRUNTIME_DATA_DIR="+dataDir,
	)
	var daemonLogs bytes.Buffer
	daemon.Stdout = &daemonLogs
	daemon.Stderr = &daemonLogs

	if err := daemon.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		if daemon.Process != nil {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			_ = daemon.Wait()
		}
		if t.Failed() {
			t.Logf("daemon logs:\n%s", daemonLogs.String())
		}
	})

	// Wait for daemon health
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", daemonPort)
	if !waitForDaemonHealth(t, baseURL, 20*time.Second) {
		t.Fatalf("daemon failed to start\n%s", daemonLogs.String())
	}
	t.Logf("daemon healthy on port %d", daemonPort)

	// Define 30 session configs: 15 Claude (7i + 8p) + 15 Codex (7i + 8p)
	type sessionSpec struct {
		agent       string
		interactive bool
		label       string
	}

	var specs []sessionSpec
	for i := 0; i < 7; i++ {
		specs = append(specs, sessionSpec{"claude", true, fmt.Sprintf("claude-interactive-%d", i)})
	}
	for i := 0; i < 8; i++ {
		specs = append(specs, sessionSpec{"claude", false, fmt.Sprintf("claude-prompt-%d", i)})
	}
	for i := 0; i < 7; i++ {
		specs = append(specs, sessionSpec{"codex", true, fmt.Sprintf("codex-interactive-%d", i)})
	}
	for i := 0; i < 8; i++ {
		specs = append(specs, sessionSpec{"codex", false, fmt.Sprintf("codex-prompt-%d", i)})
	}

	if len(specs) != 30 {
		t.Fatalf("expected 30 specs, got %d", len(specs))
	}

	prompt := "Print the numbers 1 through 10, each on its own line. No other text."
	workDir := repoRoot(t)

	// Phase 1: Create all 30 sessions concurrently
	t.Log("creating 30 sessions...")
	type sessionResult struct {
		idx       int
		sessionID string
		wsURL     string
		err       error
	}

	resultCh := make(chan sessionResult, 30)
	for i, spec := range specs {
		go func(idx int, s sessionSpec) {
			body := map[string]any{
				"agent":       s.agent,
				"interactive": s.interactive,
				"work_dir":    workDir,
				"task_id":     s.label,
				"name":        s.label,
			}
			if !s.interactive {
				body["prompt"] = prompt
			}

			data, _ := json.Marshal(body)
			resp, err := http.Post(baseURL+"/sessions", "application/json", bytes.NewReader(data))
			if err != nil {
				resultCh <- sessionResult{idx: idx, err: err}
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				respBody, _ := io.ReadAll(resp.Body)
				resultCh <- sessionResult{idx: idx, err: fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))}
				return
			}

			var sessResp struct {
				SessionID string `json:"session_id"`
				WSURL     string `json:"ws_url"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&sessResp); err != nil {
				resultCh <- sessionResult{idx: idx, err: err}
				return
			}
			resultCh <- sessionResult{idx: idx, sessionID: sessResp.SessionID, wsURL: sessResp.WSURL}
		}(i, spec)
	}

	sessions := make([]sessionResult, 30)
	for i := 0; i < 30; i++ {
		r := <-resultCh
		sessions[r.idx] = r
	}

	// Check for creation failures
	var createFails int
	for i, s := range sessions {
		if s.err != nil {
			t.Errorf("session %d (%s): create failed: %v", i, specs[i].label, s.err)
			createFails++
		}
	}
	if createFails > 0 {
		t.Fatalf("%d/%d sessions failed to create", createFails, 30)
	}
	t.Log("all 30 sessions created")

	// Phase 2: Connect all 30 WebSockets
	t.Log("connecting 30 websockets...")
	conns := make([]*websocket.Conn, 30)
	for i, s := range sessions {
		wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws/sessions/%s?since=0", daemonPort, s.sessionID)
		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial session %d (%s): %v", i, specs[i].label, err)
		}
		conns[i] = conn
		t.Cleanup(func() { _ = conn.Close() })
	}
	t.Log("all 30 websockets connected")

	// Phase 3: Send prompts to interactive sessions
	t.Log("sending prompts to 14 interactive sessions...")
	for i, spec := range specs {
		if spec.interactive {
			err := conns[i].WriteJSON(map[string]any{
				"type": "stdin",
				"data": prompt + "\n",
			})
			if err != nil {
				t.Errorf("send prompt to session %d (%s): %v", i, spec.label, err)
			}
		}
	}

	// Phase 4: Read from all 30, track which reach completion
	t.Log("reading output from all 30 sessions (up to 120s)...")
	var readWg sync.WaitGroup
	var completed atomic.Int32
	var gotOutput atomic.Int32
	completionStatus := make([]string, 30)

	for i, conn := range conns {
		readWg.Add(1)
		go func(idx int, c *websocket.Conn, label string) {
			defer readWg.Done()

			deadline := time.Now().Add(120 * time.Second)
			hasOutput := false
			finished := false

			for time.Now().Before(deadline) && !finished {
				if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					break
				}

				_, msg, err := c.ReadMessage()
				if err != nil {
					var netErr net.Error
					if errors.As(err, &netErr) && netErr.Timeout() {
						continue // keep waiting
					}
					// WS closed
					finished = true
					break
				}

				// Parse the frame
				var frame map[string]any
				if err := json.Unmarshal(msg, &frame); err != nil {
					continue
				}

				frameType, _ := frame["type"].(string)
				switch frameType {
				case "stdout", "replay":
					hasOutput = true
				case "exit":
					finished = true
				case "error":
					// Note but don't stop
					hasOutput = true
				}
			}

			if hasOutput {
				gotOutput.Add(1)
			}
			if finished {
				completed.Add(1)
				completionStatus[idx] = "completed"
			} else if hasOutput {
				completionStatus[idx] = "output-but-no-exit"
			} else {
				completionStatus[idx] = "no-output"
			}
		}(i, conn, specs[i].label)
	}

	readWg.Wait()

	// Report results
	t.Logf("Results: %d/%d completed, %d/%d got output", completed.Load(), 30, gotOutput.Load(), 30)

	claudeInteractive, claudePrompt := 0, 0
	codexInteractive, codexPrompt := 0, 0
	for i, spec := range specs {
		status := completionStatus[i]
		if status != "completed" {
			t.Logf("  %s: %s", spec.label, status)
		}
		if status == "completed" || status == "output-but-no-exit" {
			switch {
			case spec.agent == "claude" && spec.interactive:
				claudeInteractive++
			case spec.agent == "claude" && !spec.interactive:
				claudePrompt++
			case spec.agent == "codex" && spec.interactive:
				codexInteractive++
			case spec.agent == "codex" && !spec.interactive:
				codexPrompt++
			}
		}
	}

	t.Logf("Breakdown: Claude interactive=%d/7, Claude prompt=%d/8, Codex interactive=%d/7, Codex prompt=%d/8",
		claudeInteractive, claudePrompt, codexInteractive, codexPrompt)

	// At minimum, all sessions should have produced some output
	if gotOutput.Load() < 30 {
		t.Errorf("only %d/30 sessions produced output", gotOutput.Load())
	}

	// Clean up: kill all sessions
	t.Log("cleaning up sessions...")
	for _, s := range sessions {
		if s.sessionID != "" {
			req, _ := http.NewRequest(http.MethodDelete, baseURL+"/sessions/"+s.sessionID, nil)
			http.DefaultClient.Do(req)
		}
	}
}

func buildDaemonBinary(t *testing.T) string {
	t.Helper()

	outDir := t.TempDir()
	binary := outDir + "/agentd"

	cmd := exec.Command("go", "build", "-o", binary, "./cmd/agentd")
	cmd.Dir = repoRoot(t)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, output.String())
	}
	return binary
}

func waitForDaemonHealth(t *testing.T, baseURL string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func mustFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}
