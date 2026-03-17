package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	bridgepkg "github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/session"
)

const (
	defaultPort      = "9090"
	replayBufferSize = 1 << 20
	pingInterval     = 30 * time.Second
	pongTimeout      = 10 * time.Second
	writeTimeout     = 5 * time.Second
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

type healthResponse struct {
	Status       string `json:"status"`
	AgentRunning bool   `json:"agent_running"`
}

type sidecar struct {
	cmd       []string
	sessionID string

	startOnce sync.Once
	startErr  error

	mu       sync.RWMutex
	process  *exec.Cmd
	stdin    io.WriteCloser
	replay   *session.ReplayBuffer
	started  bool
	exited   bool
	exitCode int
	exitDone chan struct{}
}

type wsWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type waitResult struct {
	data []byte
	next int64
	done bool
}

func main() {
	sc, port, err := newSidecarFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("sidecar listening on :%s", port)
	if err := http.ListenAndServe(":"+port, sc.routes()); err != nil {
		log.Fatal(err)
	}
}

func newSidecarFromEnv() (*sidecar, string, error) {
	cmd, err := parseAgentCommand(os.Getenv("AGENT_CMD"))
	if err != nil {
		return nil, "", err
	}

	port := os.Getenv("SIDECAR_PORT")
	if port == "" {
		port = defaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, "", errors.New("SIDECAR_PORT must be numeric")
	}

	return newSidecar(cmd), port, nil
}

func newSidecar(cmd []string) *sidecar {
	return &sidecar{
		cmd:       append([]string(nil), cmd...),
		sessionID: uuid.New().String(),
	}
}

func parseAgentCommand(raw string) ([]string, error) {
	if raw == "" {
		return nil, errors.New("AGENT_CMD is required")
	}

	var cmd []string
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		return nil, err
	}
	if len(cmd) == 0 || cmd[0] == "" {
		return nil, errors.New("AGENT_CMD must contain a command")
	}
	return cmd, nil
}

func (s *sidecar) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ws", s.handleWS)
	return mux
}

func (s *sidecar) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:       "ok",
		AgentRunning: s.agentRunning(),
	})
}

func (s *sidecar) handleWS(w http.ResponseWriter, r *http.Request) {
	since, hasSince, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		http.Error(w, "invalid since", http.StatusBadRequest)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	startedNow, err := s.ensureStarted()
	if err != nil {
		writer := &wsWriter{conn: conn}
		_ = writer.writeJSON(bridgepkg.ServerFrame{
			Type:  "error",
			Error: err.Error(),
		})
		conn.Close()
		return
	}

	s.serveConn(conn, hasSince, since, startedNow)
}

func parseSince(raw string) (int64, bool, error) {
	if raw == "" {
		return 0, false, nil
	}
	since, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || since < 0 {
		return 0, false, errors.New("since must be a non-negative integer")
	}
	return since, true, nil
}

func (s *sidecar) ensureStarted() (bool, error) {
	s.mu.RLock()
	alreadyStarted := s.started
	s.mu.RUnlock()
	if alreadyStarted {
		return false, nil
	}

	startedNow := false
	s.startOnce.Do(func() {
		startedNow = true
		s.startErr = s.startProcess()
	})
	return startedNow, s.startErr
}

func (s *sidecar) startProcess() error {
	cmd := exec.Command(s.cmd[0], s.cmd[1:]...)

	// Detect prompt mode: if command contains -p or --print, it's a
	// one-shot prompt, not an interactive session.
	isPromptMode := false
	for _, arg := range s.cmd {
		if arg == "-p" || arg == "--print" {
			isPromptMode = true
			break
		}
	}

	replay := session.NewReplayBuffer(replayBufferSize)
	exitDone := make(chan struct{})

	var stdin io.WriteCloser
	var drainWg sync.WaitGroup

	if isPromptMode {
		// Prompt mode: use pipes, close stdin after start.
		var err error
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		stdin.Close()
		stdin = nil
		drainToReplay(stdout, replay, &drainWg)
		drainToReplay(stderr, replay, &drainWg)
	} else {
		// Interactive mode: allocate a PTY so the agent thinks it's on a
		// real terminal. This is required for Claude and Codex REPL modes.
		//
		// Ensure the working directory has a .git folder — Claude's trust
		// prompt only appears in non-git directories. A bare git init
		// satisfies this check without affecting the actual workspace.
		if wd := cmd.Dir; wd != "" {
			gitDir := filepath.Join(wd, ".git")
			if _, err := os.Stat(gitDir); os.IsNotExist(err) {
				_ = os.MkdirAll(gitDir, 0o755)
			}
		} else {
			// Default working dir
			gitDir := "/workspace/.git"
			if _, err := os.Stat(gitDir); os.IsNotExist(err) {
				_ = os.MkdirAll(gitDir, 0o755)
			}
		}

		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
			Rows: 50,
			Cols: 200,
			X:    0,
			Y:    0,
		})
		if err != nil {
			return err
		}
		// PTY merges stdin+stdout+stderr into one file descriptor (ptmx).
		// Reads from ptmx = agent output. Writes to ptmx = agent stdin.
		stdin = &nopWriteCloser{ptmx}
		drainToReplay(io.NopCloser(ptmx), replay, &drainWg)
	}

	s.mu.Lock()
	s.process = cmd
	s.stdin = stdin
	s.replay = replay
	s.started = true
	s.exitDone = exitDone
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		drainWg.Wait()
		replay.Close()

		code := exitCodeFromWait(err, cmd)

		s.mu.Lock()
		s.exited = true
		s.exitCode = code
		s.stdin = nil
		s.mu.Unlock()
		close(exitDone)
	}()

	return nil
}

func drainToReplay(r io.ReadCloser, replay *session.ReplayBuffer, wg *sync.WaitGroup) {
	if r == nil {
		return
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer r.Close()

		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_, _ = replay.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
}

func exitCodeFromWait(waitErr error, cmd *exec.Cmd) int {
	if cmd != nil && cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code
		}
	}

	if waitErr == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func (s *sidecar) serveConn(conn *websocket.Conn, hasSince bool, since int64, startedNow bool) {
	defer conn.Close()

	writer := &wsWriter{conn: conn}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
	})

	replay := s.replayBuffer()
	offset := replay.TotalBytes()
	if startedNow {
		offset = 0
	}
	if hasSince {
		data, nextOffset := replay.ReadFrom(since)
		if len(data) > 0 {
			if err := writer.writeJSON(bridgepkg.ServerFrame{
				Type:   "replay",
				Data:   base64.StdEncoding.EncodeToString(data),
				Offset: nextOffset,
			}); err != nil {
				return
			}
		}
		offset = nextOffset
	}

	if err := writer.writeJSON(bridgepkg.ServerFrame{
		Type:      "connected",
		SessionID: s.sessionID,
		Mode:      "pipe",
	}); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.pingLoop(ctx, conn, writer, cancel)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.streamLoop(ctx, replay, writer, offset, cancel)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.readLoop(ctx, conn, writer, cancel)
	}()

	wg.Wait()
}

func (s *sidecar) pingLoop(ctx context.Context, conn *websocket.Conn, writer *wsWriter, cancel context.CancelFunc) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writer.writePing(); err != nil {
				cancel()
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
		}
	}
}

func (s *sidecar) streamLoop(ctx context.Context, replay *session.ReplayBuffer, writer *wsWriter, offset int64, cancel context.CancelFunc) {
	for {
		result, ok := waitForReplay(ctx, replay, offset)
		if !ok {
			return
		}

		if len(result.data) > 0 {
			frameData := string(result.data)
			if !utf8.Valid(result.data) {
				frameData = base64.StdEncoding.EncodeToString(result.data)
			}
			if err := writer.writeJSON(bridgepkg.ServerFrame{
				Type:   "stdout",
				Data:   frameData,
				Offset: result.next,
			}); err != nil {
				cancel()
				return
			}
		}
		offset = result.next

		if result.done {
			code := s.currentExitCode()
			if err := writer.writeJSON(bridgepkg.ServerFrame{
				Type:     "exit",
				ExitCode: &code,
			}); err != nil {
				cancel()
				return
			}
			cancel()
			return
		}
	}
}

func waitForReplay(ctx context.Context, replay *session.ReplayBuffer, offset int64) (waitResult, bool) {
	resultCh := make(chan waitResult, 1)
	go func() {
		data, next, done := replay.WaitFor(offset)
		resultCh <- waitResult{data: data, next: next, done: done}
	}()

	select {
	case <-ctx.Done():
		return waitResult{}, false
	case result := <-resultCh:
		return result, true
	}
}

func (s *sidecar) readLoop(ctx context.Context, conn *websocket.Conn, writer *wsWriter, cancel context.CancelFunc) {
	go func() {
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()

	for {
		var frame bridgepkg.ClientFrame
		if err := conn.ReadJSON(&frame); err != nil {
			cancel()
			return
		}

		_ = conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))

		switch frame.Type {
		case "stdin":
			stdin := s.stdinWriter()
			if stdin == nil {
				_ = writer.writeJSON(bridgepkg.ServerFrame{
					Type:  "error",
					Error: "stdin unavailable",
				})
				cancel()
				return
			}
			if _, err := io.WriteString(stdin, frame.Data); err != nil {
				_ = writer.writeJSON(bridgepkg.ServerFrame{
					Type:  "error",
					Error: err.Error(),
				})
				cancel()
				return
			}
		case "ping":
			if err := writer.writeJSON(bridgepkg.ServerFrame{Type: "pong"}); err != nil {
				cancel()
				return
			}
		case "resize":
			// PTY resize is not supported by the in-container sidecar yet.
		default:
			if err := writer.writeJSON(bridgepkg.ServerFrame{
				Type:  "error",
				Error: "unknown frame type: " + frame.Type,
			}); err != nil {
				cancel()
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (w *wsWriter) writeJSON(frame bridgepkg.ServerFrame) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return w.conn.WriteJSON(frame)
}

func (w *wsWriter) writePing() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return w.conn.WriteMessage(websocket.PingMessage, nil)
}

func (s *sidecar) replayBuffer() *session.ReplayBuffer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.replay
}

func (s *sidecar) stdinWriter() io.WriteCloser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stdin
}

func (s *sidecar) currentExitCode() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exitCode
}

func (s *sidecar) agentRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.started && !s.exited
}

func (s *sidecar) stop() {
	s.mu.RLock()
	process := s.process
	exitDone := s.exitDone
	running := s.started && !s.exited
	s.mu.RUnlock()

	if !running || process == nil || process.Process == nil {
		return
	}

	_ = process.Process.Kill()
	if exitDone == nil {
		return
	}

	select {
	case <-exitDone:
	case <-time.After(2 * time.Second):
	}
}

// nopWriteCloser wraps an *os.File as io.WriteCloser.
// Close is a no-op because the PTY fd is managed by the process lifecycle.
type nopWriteCloser struct {
	f *os.File
}

func (w *nopWriteCloser) Write(p []byte) (int, error) { return w.f.Write(p) }
func (w *nopWriteCloser) Close() error                { return nil }
