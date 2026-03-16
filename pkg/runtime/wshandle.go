package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const wsHandlePingInterval = 30 * time.Second

type wsHandle struct {
	conn        *websocket.Conn
	stdinR      *io.PipeReader
	stdinW      *io.PipeWriter
	stdoutR     *io.PipeReader
	stdoutW     *io.PipeWriter
	done        chan ExitResult
	containerID string
	hostPort    string
	cancel      context.CancelFunc
	metaMu      sync.RWMutex
	cleanup     func()
	finished    bool
	cleanupDone bool
	recovery    *RecoveryInfo
}

type wsServerFrame struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

type wsClientFrame struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

func newWSHandle(conn *websocket.Conn, containerID, hostPort string) *wsHandle {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	handle := &wsHandle{
		conn:        conn,
		stdinR:      stdinR,
		stdinW:      stdinW,
		stdoutR:     stdoutR,
		stdoutW:     stdoutW,
		done:        make(chan ExitResult, 1),
		containerID: containerID,
		hostPort:    hostPort,
		cancel:      cancel,
	}

	var finishOnce sync.Once
	var writeMu sync.Mutex
	finish := func(result ExitResult) {
		finishOnce.Do(func() {
			handle.done <- result
			_ = handle.stdinW.Close()
			_ = handle.stdoutW.Close()
			cancel()
			if handle.conn != nil {
				_ = handle.conn.Close()
			}
			handle.runCleanup()
		})
	}

	go func() {
		for {
			var frame wsServerFrame
			if err := handle.conn.ReadJSON(&frame); err != nil {
				if ctx.Err() == nil {
					finish(ExitResult{Err: err})
				}
				return
			}

			switch frame.Type {
			case "stdout", "replay":
				if _, err := handle.stdoutW.Write([]byte(frame.Data)); err != nil {
					if ctx.Err() == nil {
						finish(ExitResult{Err: err})
					}
					return
				}
			case "exit":
				code := 0
				if frame.ExitCode != nil {
					code = *frame.ExitCode
				}
				finish(ExitResult{Code: code})
				return
			case "connected", "pong":
				continue
			}
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := handle.stdinR.Read(buf)
			if n > 0 {
				writeMu.Lock()
				writeErr := handle.conn.WriteJSON(wsClientFrame{
					Type: "stdin",
					Data: string(buf[:n]),
				})
				writeMu.Unlock()
				if writeErr != nil {
					if ctx.Err() == nil {
						finish(ExitResult{Err: writeErr})
					}
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					return
				}
				finish(ExitResult{Err: err})
				return
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(wsHandlePingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				err := handle.conn.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
				if err != nil {
					if ctx.Err() == nil {
						finish(ExitResult{Err: err})
					}
					return
				}
			}
		}
	}()

	return handle
}

func (h *wsHandle) setCleanup(cleanup func()) {
	var runCleanup func()
	h.metaMu.Lock()
	h.cleanup = cleanup
	if h.finished && !h.cleanupDone && h.cleanup != nil {
		h.cleanupDone = true
		runCleanup = h.cleanup
	}
	h.metaMu.Unlock()
	if runCleanup != nil {
		runCleanup()
	}
}

func (h *wsHandle) setRecoveryInfo(info *RecoveryInfo) {
	h.metaMu.Lock()
	defer h.metaMu.Unlock()
	h.recovery = info
}

func (h *wsHandle) runCleanup() {
	var cleanup func()
	h.metaMu.Lock()
	h.finished = true
	if !h.cleanupDone && h.cleanup != nil {
		h.cleanupDone = true
		cleanup = h.cleanup
	}
	h.metaMu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func dialSidecar(containerID, hostPort string, sinceOffset int64) (*wsHandle, error) {
	u := url.URL{
		Scheme: "ws",
		Host:   "localhost:" + hostPort,
		Path:   "/ws",
	}
	q := u.Query()
	q.Set("since", fmt.Sprintf("%d", sinceOffset))
	u.RawQuery = q.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, err
	}

	return newWSHandle(conn, containerID, hostPort), nil
}

func (h *wsHandle) Stdin() io.WriteCloser   { return h.stdinW }
func (h *wsHandle) Stdout() io.ReadCloser   { return h.stdoutR }
func (h *wsHandle) Stderr() io.ReadCloser   { return nil }
func (h *wsHandle) Wait() <-chan ExitResult { return h.done }

func (h *wsHandle) Kill() error {
	stopErr := exec.Command("docker", "stop", h.containerID).Run()
	rmErr := exec.Command("docker", "rm", h.containerID).Run()

	if h.cancel != nil {
		h.cancel()
	}
	if h.stdinW != nil {
		_ = h.stdinW.Close()
	}
	if h.conn != nil {
		_ = h.conn.Close()
	}
	h.runCleanup()

	return errors.Join(stopErr, rmErr)
}

func (h *wsHandle) PID() int { return 0 }

func (h *wsHandle) RecoveryInfo() *RecoveryInfo {
	h.metaMu.RLock()
	defer h.metaMu.RUnlock()
	return h.recovery
}
