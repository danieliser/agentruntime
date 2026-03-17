package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	bridgepkg "github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/session"
)

type legacyPTYSidecar struct {
	cmd       []string
	agentType string
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

type legacyWSWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type legacyWaitResult struct {
	data []byte
	next int64
	done bool
}

func newLegacyPTYSidecar(cmd []string) *legacyPTYSidecar {
	return &legacyPTYSidecar{
		cmd:       append([]string(nil), cmd...),
		agentType: detectAgentType(cmd),
		sessionID: uuid.NewString(),
	}
}

func (s *legacyPTYSidecar) AgentType() string {
	return s.agentType
}

func (s *legacyPTYSidecar) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ws", s.handleWS)
	return mux
}

func (s *legacyPTYSidecar) Close() error {
	s.stop()
	return nil
}

func (s *legacyPTYSidecar) Interrupt() error {
	s.stop()
	return nil
}

func (s *legacyPTYSidecar) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:       "ok",
		AgentRunning: s.agentRunning(),
		AgentType:    s.agentType,
		SessionID:    s.sessionID,
	})
}

func (s *legacyPTYSidecar) handleWS(w http.ResponseWriter, r *http.Request) {
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
		writer := &legacyWSWriter{conn: conn}
		_ = writer.writeJSON(bridgepkg.ServerFrame{
			Type:  "error",
			Error: err.Error(),
		})
		_ = conn.Close()
		return
	}

	s.serveConn(conn, hasSince, since, startedNow)
}

func (s *legacyPTYSidecar) ensureStarted() (bool, error) {
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

func (s *legacyPTYSidecar) startProcess() error {
	cmd := exec.Command(s.cmd[0], s.cmd[1:]...)

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
		_ = stdin.Close()
		stdin = nil
		drainToReplay(stdout, replay, &drainWg)
		drainToReplay(stderr, replay, &drainWg)
	} else {
		if wd := cmd.Dir; wd != "" {
			gitDir := filepath.Join(wd, ".git")
			if _, err := os.Stat(gitDir); os.IsNotExist(err) {
				_ = os.MkdirAll(gitDir, 0o755)
			}
		} else {
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
		stdin = &nopWriteCloser{f: ptmx}
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

func (s *legacyPTYSidecar) serveConn(conn *websocket.Conn, hasSince bool, since int64, startedNow bool) {
	defer conn.Close()

	writer := &legacyWSWriter{conn: conn}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
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

func (s *legacyPTYSidecar) pingLoop(ctx context.Context, conn *websocket.Conn, writer *legacyWSWriter, cancel context.CancelFunc) {
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

func (s *legacyPTYSidecar) streamLoop(ctx context.Context, replay *session.ReplayBuffer, writer *legacyWSWriter, offset int64, cancel context.CancelFunc) {
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

func waitForReplay(ctx context.Context, replay *session.ReplayBuffer, offset int64) (legacyWaitResult, bool) {
	resultCh := make(chan legacyWaitResult, 1)
	go func() {
		data, next, done := replay.WaitFor(offset)
		resultCh <- legacyWaitResult{data: data, next: next, done: done}
	}()

	select {
	case <-ctx.Done():
		return legacyWaitResult{}, false
	case result := <-resultCh:
		return result, true
	}
}

func (s *legacyPTYSidecar) readLoop(ctx context.Context, conn *websocket.Conn, writer *legacyWSWriter, cancel context.CancelFunc) {
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

func (w *legacyWSWriter) writeJSON(frame bridgepkg.ServerFrame) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return w.conn.WriteJSON(frame)
}

func (w *legacyWSWriter) writePing() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return w.conn.WriteMessage(websocket.PingMessage, nil)
}

func (s *legacyPTYSidecar) replayBuffer() *session.ReplayBuffer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.replay
}

func (s *legacyPTYSidecar) stdinWriter() io.WriteCloser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stdin
}

func (s *legacyPTYSidecar) currentExitCode() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exitCode
}

func (s *legacyPTYSidecar) agentRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.started && !s.exited
}

func (s *legacyPTYSidecar) stop() {
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

type nopWriteCloser struct {
	f *os.File
}

func (w *nopWriteCloser) Write(p []byte) (int, error) { return w.f.Write(p) }
func (w *nopWriteCloser) Close() error                { return nil }
