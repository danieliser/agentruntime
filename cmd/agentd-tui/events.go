package main

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

var debugLog *log.Logger

func init() {
	f, err := os.OpenFile("/tmp/agentd-tui.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		debugLog = log.New(os.Stderr, "[tui] ", log.LstdFlags)
		return
	}
	debugLog = log.New(f, "[tui] ", log.LstdFlags|log.Lmicroseconds)
}

// Bridge frame from daemon WS.
type serverFrame struct {
	Type      string `json:"type"`
	Data      string `json:"data,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Parsed NDJSON event from sidecar (carried inside stdout/replay frames).
type agentEvent struct {
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
	Offset    int64                  `json:"offset"`
	Timestamp int64                  `json:"timestamp"`
}

// Tea messages produced by the WS pump.
type (
	connectedMsg   struct{ sessionID string }
	agentEventMsg  struct{ event agentEvent; replay bool }
	sessionExitMsg struct{ code int }
	wsErrorMsg     struct{ err error }
)

// pumpEvents reads frames from the WS and sends them as tea.Msg to the program.
func pumpEvents(conn *websocket.Conn, p *tea.Program) {
	defer conn.Close()
	debugLog.Println("pumpEvents started")

	for {
		var frame serverFrame
		if err := conn.ReadJSON(&frame); err != nil {
			debugLog.Printf("WS read error: %v", err)
			p.Send(wsErrorMsg{err: err})
			return
		}

		debugLog.Printf("frame: type=%s data_len=%d", frame.Type, len(frame.Data))

		switch frame.Type {
		case "connected":
			p.Send(connectedMsg{sessionID: frame.SessionID})

		case "replay", "stdout":
			isReplay := frame.Type == "replay"
			// Try base64 first (legacy bridge), fall back to raw NDJSON.
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				data = []byte(frame.Data)
			}
			// Parse NDJSON lines.
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var ev agentEvent
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					debugLog.Printf("NDJSON parse error: %v line=%s", err, line[:min(len(line), 100)])
					continue
				}
				debugLog.Printf("event: type=%s replay=%v", ev.Type, isReplay)
				p.Send(agentEventMsg{event: ev, replay: isReplay})
			}

		case "exit":
			code := -1
			if frame.ExitCode != nil {
				code = *frame.ExitCode
			}
			debugLog.Printf("exit frame: code=%d", code)
			p.Send(sessionExitMsg{code: code})
			return

		case "error":
			debugLog.Printf("error frame: %s", frame.Error)
			p.Send(agentEventMsg{
				event: agentEvent{
					Type: "error",
					Data: map[string]interface{}{"error_detail": frame.Error},
				},
			})
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
