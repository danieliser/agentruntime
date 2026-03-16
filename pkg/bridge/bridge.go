package bridge

import (
	"context"
	"encoding/base64"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

const (
	pingInterval = 30 * time.Second
	pongTimeout  = 10 * time.Second
	writeTimeout = 5 * time.Second
	readTimeout  = 60 * time.Second
)

// Bridge connects a session's replay buffer to a WebSocket connection.
// Output flows: process → drain goroutine → ReplayBuffer → Bridge → WS client.
// The bridge never reads process pipes directly — it subscribes to the replay
// buffer via WaitFor, which blocks until new data arrives or the buffer is closed.
type Bridge struct {
	conn    *websocket.Conn
	handle  runtime.ProcessHandle
	replay  *session.ReplayBuffer
	writeMu sync.Mutex
	cancel  context.CancelFunc
}

// New creates a new bridge. Call Run() to start the I/O loops.
func New(conn *websocket.Conn, handle runtime.ProcessHandle, replay *session.ReplayBuffer) *Bridge {
	return &Bridge{
		conn:   conn,
		handle: handle,
		replay: replay,
	}
}

// Run starts the bridge I/O loops. Blocks until the replay buffer is closed
// (process exited and drains finished) or the WebSocket disconnects.
// Sends replay of buffered data if sinceOffset >= 0.
func (b *Bridge) Run(ctx context.Context, sessionID string, sinceOffset int64) {
	ctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	defer cancel()
	defer b.conn.Close()

	// Determine starting offset for the stream pump.
	var streamOffset int64
	if sinceOffset >= 0 {
		streamOffset = sinceOffset
		// Send initial replay chunk.
		data, nextOffset := b.replay.ReadFrom(sinceOffset)
		if len(data) > 0 {
			_ = b.writeJSON(ServerFrame{
				Type:   "replay",
				Data:   base64.StdEncoding.EncodeToString(data),
				Offset: nextOffset,
			})
		}
		streamOffset = nextOffset
	} else {
		// No replay requested — start streaming from current position.
		streamOffset = b.replay.TotalBytes()
	}

	// Send connected frame.
	_ = b.writeJSON(ServerFrame{
		Type:      "connected",
		SessionID: sessionID,
		Mode:      "pipe",
	})

	// Set pong handler for keepalive.
	b.conn.SetPongHandler(func(string) error {
		b.conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})

	var wg sync.WaitGroup

	// Ping/pong keepalive.
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.pingLoop(ctx)
	}()

	// Stream pump — reads from replay buffer (not process pipes).
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.replayStreamPump(ctx, streamOffset)
	}()

	// Stdin pump (runs on this goroutine — blocks until WS closes or ctx cancelled).
	b.stdinPump(ctx)

	// On WS disconnect, cancel all goroutines.
	cancel()
	wg.Wait()
}

// replayStreamPump subscribes to the replay buffer and sends new data as
// stdout frames until the buffer is closed or the context is cancelled.
func (b *Bridge) replayStreamPump(ctx context.Context, offset int64) {
	// Unblock WaitFor when context is cancelled.
	go func() {
		<-ctx.Done()
		b.replay.Close()
	}()

	for {
		data, nextOffset, done := b.replay.WaitFor(offset)

		// Check context first — we may have been woken by Close from cancellation.
		if ctx.Err() != nil {
			return
		}

		if len(data) > 0 {
			frameData := string(data)
			if !utf8.Valid(data) {
				frameData = base64.StdEncoding.EncodeToString(data)
			}
			if err := b.writeJSON(ServerFrame{
				Type:   "stdout",
				Data:   frameData,
				Offset: nextOffset,
			}); err != nil {
				return
			}
		}
		offset = nextOffset

		if done {
			// Buffer closed — process is done. Send exit frame.
			// Get exit code from the process handle.
			select {
			case result := <-b.handle.Wait():
				code := result.Code
				_ = b.writeJSON(ServerFrame{
					Type:     "exit",
					ExitCode: &code,
				})
			default:
				// Process already waited — send exit 0 as fallback.
				code := 0
				_ = b.writeJSON(ServerFrame{
					Type:     "exit",
					ExitCode: &code,
				})
			}
			b.cancel()
			return
		}
	}
}

// stdinPump reads client frames and routes them to the process stdin.
func (b *Bridge) stdinPump(ctx context.Context) {
	// Unblock ReadJSON on context cancellation.
	go func() {
		<-ctx.Done()
		b.conn.SetReadDeadline(time.Now())
	}()

	b.conn.SetReadDeadline(time.Now().Add(readTimeout))
	for {
		var frame ClientFrame
		if err := b.conn.ReadJSON(&frame); err != nil {
			if ctx.Err() == nil {
				b.cancel()
			}
			return
		}
		b.conn.SetReadDeadline(time.Now().Add(readTimeout))

		switch frame.Type {
		case "stdin":
			if stdin := b.handle.Stdin(); stdin != nil {
				if _, err := stdin.Write([]byte(frame.Data)); err != nil {
					_ = b.writeJSON(ServerFrame{
						Type:  "error",
						Error: err.Error(),
					})
					b.cancel()
					return
				}
			}
		case "ping":
			_ = b.writeJSON(ServerFrame{Type: "pong"})
		case "resize":
			// TODO: forward to PTY when supported
		default:
			_ = b.writeJSON(ServerFrame{
				Type:  "error",
				Error: "unknown frame type: " + frame.Type,
			})
		}
	}
}

// pingLoop sends WebSocket pings at a regular interval.
func (b *Bridge) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.writeMu.Lock()
			b.conn.SetWriteDeadline(time.Now().Add(pongTimeout))
			err := b.conn.WriteMessage(websocket.PingMessage, nil)
			b.writeMu.Unlock()
			if err != nil {
				b.cancel()
				return
			}
		}
	}
}

// writeJSON serializes a frame to the WebSocket with write locking.
func (b *Bridge) writeJSON(v ServerFrame) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return b.conn.WriteJSON(v)
}
