package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

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

type streamReadyFrame struct {
	FrameType     string `json:"frame_type"`
	SessionID     string `json:"session_id"`
	AfterSequence int64  `json:"after_sequence"`
	ReplayThrough int64  `json:"replay_through"`
	Error         struct {
		Message string `json:"message"`
	} `json:"error"`
}

type durableEvent struct {
	SessionID string                 `json:"session_id"`
	Sequence  int64                  `json:"sequence"`
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
	Stream    string                 `json:"stream"`
	Payload   map[string]interface{} `json:"payload"`
}

type agentEvent struct {
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
	Offset    int64                  `json:"offset"`
	Timestamp int64                  `json:"timestamp"`
}

type (
	connectedMsg  struct{ sessionID string }
	agentEventMsg struct {
		event  agentEvent
		replay bool
	}
	sessionExitMsg struct{ code int }
	wsErrorMsg     struct{ err error }
)

func pumpEvents(conn *websocket.Conn, p *tea.Program) {
	defer conn.Close()
	debugLog.Println("pumpEvents started")
	var cursor int64
	var replayThrough int64
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			debugLog.Printf("WS read error: %v", err)
			p.Send(wsErrorMsg{err: err})
			return
		}
		var ready streamReadyFrame
		if err := json.Unmarshal(raw, &ready); err != nil {
			p.Send(wsErrorMsg{err: fmt.Errorf("decode event frame: %w", err)})
			return
		}
		switch ready.FrameType {
		case "stream.ready":
			cursor = ready.AfterSequence
			replayThrough = ready.ReplayThrough
			p.Send(connectedMsg{sessionID: ready.SessionID})
			continue
		case "error":
			p.Send(wsErrorMsg{err: fmt.Errorf("event stream: %s", ready.Error.Message)})
			return
		}

		var event durableEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			p.Send(wsErrorMsg{err: fmt.Errorf("decode durable event: %w", err)})
			return
		}
		if event.Sequence != cursor+1 {
			p.Send(wsErrorMsg{err: fmt.Errorf("event sequence gap: got %d after %d", event.Sequence, cursor)})
			return
		}
		cursor = event.Sequence
		if event.Stream == "terminal" {
			code := -1
			if value, ok := event.Payload["exit_code"].(float64); ok {
				code = int(value)
			}
			p.Send(sessionExitMsg{code: code})
			return
		}
		mapped, ok := mapDurableEvent(event)
		if !ok {
			continue
		}
		debugLog.Printf("event: sequence=%d type=%s replay=%v", event.Sequence, event.Type, event.Sequence <= replayThrough)
		p.Send(agentEventMsg{event: mapped, replay: event.Sequence <= replayThrough})
	}
}

func mapDurableEvent(event durableEvent) (agentEvent, bool) {
	data := event.Payload
	if data == nil {
		data = map[string]interface{}{}
	}
	typeName := ""
	switch event.Type {
	case "content.delta":
		typeName = "agent_message"
		data["delta"] = true
	case "tool.call":
		typeName = "tool_use"
	case "tool.result":
		typeName = "tool_result"
	case "usage":
		typeName = "result"
	case "runtime.stderr", "error.protocol":
		typeName = "error"
		if _, exists := data["error_detail"]; !exists {
			data["error_detail"] = data["text"]
		}
	case "control.approval.request":
		typeName = "system"
		data["subtype"] = "input_request"
	default:
		return agentEvent{}, false
	}
	return agentEvent{Type: typeName, Data: data, Offset: event.Sequence, Timestamp: event.Timestamp.UnixMilli()}, true
}
