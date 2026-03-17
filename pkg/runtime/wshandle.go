package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
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
	killFn      func() error // override for non-Docker runtimes
	writeMu     sync.Mutex
	metaMu      sync.RWMutex
	cleanup     func()
	finished    bool
	cleanupDone bool
	recovery    *RecoveryInfo
	lastError   string
}

type wsServerFrame struct {
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data,omitempty"`
	ExitCode *int            `json:"exit_code,omitempty"`
}

type wsClientFrame struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
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
					finish(ExitResult{Err: handle.wsDisconnectError(err)})
				}
				return
			}

			switch frame.Type {
			case "stdout", "replay":
				if payload, ok := wsFrameStringData(frame.Data); ok {
					if _, err := handle.stdoutW.Write([]byte(payload)); err != nil {
						if ctx.Err() == nil {
							finish(ExitResult{Err: err})
						}
						return
					}
					continue
				}
				if err := handle.writeEvent(frame); err != nil {
					if ctx.Err() == nil {
						finish(ExitResult{Err: err})
					}
					return
				}
			case "agent_message", "tool_use", "tool_result", "result", "progress", "system", "error":
				handle.rememberFrameError(frame)
				if err := handle.writeEvent(frame); err != nil {
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
				// Send as a sidecar "prompt" command, not raw "stdin".
				// The sidecar WS protocol expects typed commands:
				// prompt, interrupt, steer, context, mention.
				content := strings.TrimRight(string(buf[:n]), "\n\r")
				writeErr := handle.writeJSON(wsClientFrame{
					Type: "prompt",
					Data: map[string]string{"content": content},
				})
				if writeErr != nil {
					if ctx.Err() == nil {
						finish(ExitResult{Err: handle.wsDisconnectError(writeErr)})
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
				err := handle.writeMessage(websocket.PingMessage, nil)
				if err != nil {
					if ctx.Err() == nil {
						finish(ExitResult{Err: handle.wsDisconnectError(err)})
					}
					return
				}
			}
		}
	}()

	return handle
}

func (h *wsHandle) SendPrompt(content string) error {
	if content == "" {
		return nil
	}
	return h.writeJSON(wsClientFrame{
		Type: "prompt",
		Data: map[string]string{
			"content": content,
		},
	})
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

func (h *wsHandle) rememberFrameError(frame wsServerFrame) {
	if frame.Type != "error" {
		return
	}

	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(frame.Data, &payload); err != nil {
		return
	}
	if strings.TrimSpace(payload.Message) == "" {
		return
	}

	h.metaMu.Lock()
	h.lastError = payload.Message
	h.metaMu.Unlock()
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

func dialSidecar(containerID, hostPort string, sinceOffset int64, prompt string) (*wsHandle, error) {
	u := url.URL{
		Scheme: "ws",
		Host:   "localhost:" + hostPort,
		Path:   "/ws",
	}
	q := u.Query()
	q.Set("since", fmt.Sprintf("%d", sinceOffset))
	u.RawQuery = q.Encode()

	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, websocketDialError(err, resp)
	}

	handle := newWSHandle(conn, containerID, hostPort)
	if err := handle.SendPrompt(prompt); err != nil {
		if handle.cancel != nil {
			handle.cancel()
		}
		_ = handle.conn.Close()
		return nil, handle.wsDisconnectError(err)
	}
	return handle, nil
}

func (h *wsHandle) Stdin() io.WriteCloser   { return h.stdinW }
func (h *wsHandle) Stdout() io.ReadCloser   { return h.stdoutR }
func (h *wsHandle) Stderr() io.ReadCloser   { return nil }
func (h *wsHandle) Wait() <-chan ExitResult { return h.done }

func (h *wsHandle) Kill() error {
	var killErr error
	if h.killFn != nil {
		killErr = h.killFn()
	} else {
		// Default: Docker container stop + remove
		stopErr := exec.Command("docker", "stop", h.containerID).Run()
		rmErr := exec.Command("docker", "rm", h.containerID).Run()
		if stopErr != nil {
			killErr = stopErr
		} else {
			killErr = rmErr
		}
	}

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

	return killErr
}

func (h *wsHandle) PID() int { return 0 }

func (h *wsHandle) RecoveryInfo() *RecoveryInfo {
	h.metaMu.RLock()
	defer h.metaMu.RUnlock()
	return h.recovery
}

func (h *wsHandle) wsDisconnectError(err error) error {
	if h.shouldTreatUnexpectedCloseAsExit(err) {
		return nil
	}

	h.metaMu.RLock()
	lastError := h.lastError
	h.metaMu.RUnlock()

	if lastError != "" {
		return fmt.Errorf("sidecar websocket disconnected after error: %s", lastError)
	}
	return fmt.Errorf("sidecar websocket disconnected: %w", err)
}

func (h *wsHandle) shouldTreatUnexpectedCloseAsExit(err error) bool {
	if err == nil || h.RecoveryInfo() == nil {
		return false
	}

	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		websocket.IsCloseError(err,
			websocket.CloseNormalClosure,
			websocket.CloseGoingAway,
			websocket.CloseNoStatusReceived,
			websocket.CloseAbnormalClosure,
		)
}

func (h *wsHandle) writeEvent(frame wsServerFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = h.stdoutW.Write(payload)
	return err
}

func (h *wsHandle) writeJSON(v any) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.conn.WriteJSON(v)
}

func (h *wsHandle) writeMessage(messageType int, data []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.conn.WriteMessage(messageType, data)
}

func wsFrameStringData(data json.RawMessage) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	var payload string
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", false
	}
	return payload, true
}

func websocketDialError(err error, resp *http.Response) error {
	if resp == nil {
		return err
	}
	defer resp.Body.Close()

	detail := strings.TrimSpace(httpResponseBody(resp))
	if detail == "" {
		return fmt.Errorf("%w (status %s)", err, resp.Status)
	}
	return fmt.Errorf("%w (status %s: %s)", err, resp.Status, detail)
}

func httpResponseBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	return string(data)
}
