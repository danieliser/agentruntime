package bridge

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"sync"
	"time"

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

// Bridge connects a ProcessHandle's stdio to a WebSocket connection,
// multiplexing stdout/stderr as ServerFrames and routing ClientFrames
// to stdin. All output is simultaneously written to the replay buffer.
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

// Run starts the bridge I/O loops. Blocks until the process exits or the
// WebSocket disconnects. Sends replay frames if sinceOffset >= 0.
func (b *Bridge) Run(ctx context.Context, sessionID string, sinceOffset int64) {
	ctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	defer cancel()

	// Send replay if requested.
	if sinceOffset >= 0 {
		data, nextOffset := b.replay.ReadFrom(sinceOffset)
		if len(data) > 0 {
			_ = b.writeJSON(ServerFrame{
				Type:   "replay",
				Data:   base64.StdEncoding.EncodeToString(data),
				Offset: nextOffset,
			})
		}
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

	// ioPumpsWg tracks the stdout/stderr read pumps. exitWatch waits on this
	// before sending the exit frame to guarantee no output lines are lost.
	var ioPumpsWg sync.WaitGroup
	var allWg sync.WaitGroup

	// Ping/pong keepalive.
	allWg.Add(1)
	go func() {
		defer allWg.Done()
		b.pingLoop(ctx)
	}()

	// Stdout pump.
	stdout := b.handle.Stdout()
	if stdout != nil {
		ioPumpsWg.Add(1)
		allWg.Add(1)
		go func() {
			defer ioPumpsWg.Done()
			defer allWg.Done()
			b.readPump(ctx, stdout, "stdout")
		}()
	}

	// Stderr pump.
	stderr := b.handle.Stderr()
	if stderr != nil {
		ioPumpsWg.Add(1)
		allWg.Add(1)
		go func() {
			defer ioPumpsWg.Done()
			defer allWg.Done()
			b.readPump(ctx, stderr, "stderr")
		}()
	}

	// Exit watcher — waits for IO pumps to drain before sending the exit frame
	// so the last lines of output are never lost.
	allWg.Add(1)
	go func() {
		defer allWg.Done()
		b.exitWatch(ctx, &ioPumpsWg, stdout, stderr)
	}()

	// Stdin pump (runs on this goroutine — blocks until WS closes or ctx cancelled).
	b.stdinPump(ctx)

	// On WS disconnect (stdinPump returned due to client close), cancel ctx and
	// close pipes to unblock any blocked scanner.Scan() calls in the IO pumps.
	cancel()
	if stdout != nil {
		stdout.Close()
	}
	if stderr != nil {
		stderr.Close()
	}
	allWg.Wait()
}

// readPump reads lines from a process stream and sends them as WS frames.
func (b *Bridge) readPump(ctx context.Context, reader io.Reader, frameType string) {
	scanner := bufio.NewScanner(reader)
	// Increase buffer size for agents that emit long lines (NDJSON).
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Bytes()
		lineWithNewline := append(line, '\n')

		// Write to replay buffer.
		b.replay.Write(lineWithNewline)

		// Send to WebSocket.
		if err := b.writeJSON(ServerFrame{
			Type:   frameType,
			Data:   string(lineWithNewline),
			Offset: b.replay.Total,
		}); err != nil {
			return
		}
	}
}

// stdinPump reads client frames and routes them to the process stdin.
func (b *Bridge) stdinPump(ctx context.Context) {
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

// exitWatch waits for the process to exit, drains IO pumps, then sends the exit frame.
// Closing stdout/stderr pipes unblocks any blocked scanner.Scan() in the read pumps.
func (b *Bridge) exitWatch(ctx context.Context, ioPumpsWg *sync.WaitGroup, stdout, stderr io.ReadCloser) {
	select {
	case <-ctx.Done():
		return
	case result := <-b.handle.Wait():
		// Process exited: its write-end of stdout/stderr pipes is closed by the OS.
		// However, bufio.Scanner may still be blocked on an empty pipe. Close the
		// read ends to unblock scanners immediately, then wait for pumps to drain.
		if stdout != nil {
			stdout.Close()
		}
		if stderr != nil {
			stderr.Close()
		}
		ioPumpsWg.Wait()
		code := result.Code
		_ = b.writeJSON(ServerFrame{
			Type:     "exit",
			ExitCode: &code,
		})
		b.cancel()
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
