// Package bridge implements the WebSocket bridge that connects agent process
// stdio to WebSocket clients with frame-based multiplexing.
package bridge

import "encoding/json"

// ServerFrame is sent from the server to a WebSocket client.
type ServerFrame struct {
	Type      string `json:"type"`                // stdout, stderr, exit, replay, connected, pong, error
	Data      string `json:"data,omitempty"`       // output data (base64 for binary, plain for text)
	ExitCode  *int   `json:"exit_code,omitempty"`  // set on exit frame
	Offset    int64  `json:"offset,omitempty"`     // replay buffer offset
	SessionID string `json:"session_id,omitempty"` // set on connected frame
	Mode      string `json:"mode,omitempty"`       // "pipe" or "pty" on connected frame
	Error     string `json:"error,omitempty"`      // set on error frame
	Gap       bool   `json:"gap,omitempty"`        // true if output was lost
	Recovered bool   `json:"recovered,omitempty"`  // true if session was recovered after daemon restart
}

// ClientFrame is received from a WebSocket client.
//
// Supported types:
//   - stdin:     legacy text input (Data field)
//   - ping:      keepalive
//   - resize:    terminal resize (Cols/Rows)
//   - steer:     mid-conversation steering (Data field)
//   - interrupt: cancel current agent action
//   - context:   attach context text and/or file (Context field)
//   - mention:   reference a file location (Mention field)
type ClientFrame struct {
	Type    string              `json:"type"`              // stdin, ping, resize, steer, interrupt, context, mention
	Data    string              `json:"data,omitempty"`    // input data for stdin/steer frames
	Cols    int                 `json:"cols,omitempty"`    // terminal columns for resize
	Rows    int                 `json:"rows,omitempty"`    // terminal rows for resize
	Context *ClientFrameContext `json:"context,omitempty"` // payload for context frames
	Mention *ClientFrameMention `json:"mention,omitempty"` // payload for mention frames
}

// ClientFrameContext carries the payload for a "context" client frame.
type ClientFrameContext struct {
	Text     string `json:"text,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

// ClientFrameMention carries the payload for a "mention" client frame.
type ClientFrameMention struct {
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
}

// UnmarshalClientFrame parses a raw JSON message into a ClientFrame.
func UnmarshalClientFrame(data []byte) (ClientFrame, error) {
	var f ClientFrame
	err := json.Unmarshal(data, &f)
	return f, err
}
