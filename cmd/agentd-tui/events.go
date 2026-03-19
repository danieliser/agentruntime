package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

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

	for {
		var frame serverFrame
		if err := conn.ReadJSON(&frame); err != nil {
			p.Send(wsErrorMsg{err: err})
			return
		}

		switch frame.Type {
		case "connected":
			p.Send(connectedMsg{sessionID: frame.SessionID})

		case "replay", "stdout":
			isReplay := frame.Type == "replay"
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				continue
			}
			// Parse NDJSON lines.
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var ev agentEvent
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					continue
				}
				p.Send(agentEventMsg{event: ev, replay: isReplay})
			}

		case "exit":
			code := -1
			if frame.ExitCode != nil {
				code = *frame.ExitCode
			}
			p.Send(sessionExitMsg{code: code})
			return

		case "error":
			p.Send(agentEventMsg{
				event: agentEvent{
					Type: "error",
					Data: map[string]interface{}{"error_detail": frame.Error},
				},
			})
		}
	}
}
