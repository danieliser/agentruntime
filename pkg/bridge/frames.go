// Package bridge implements the WebSocket bridge that connects agent process
// stdio to WebSocket clients with frame-based multiplexing.
package bridge

// ServerFrame is sent from the server to a WebSocket client.
type ServerFrame struct {
	Type      string `json:"type"`                // stdout, stderr, exit, replay, connected, pong, error
	Data      string `json:"data,omitempty"`       // output data (base64 for binary, plain for text)
	ExitCode  *int   `json:"exit_code,omitempty"`  // set on exit frame
	Offset    int64  `json:"offset,omitempty"`     // replay buffer offset
	SessionID string `json:"session_id,omitempty"` // set on connected frame
	Mode      string `json:"mode,omitempty"`       // "pipe" or "pty" on connected frame
	Error     string `json:"error,omitempty"`      // set on error frame
}

// ClientFrame is received from a WebSocket client.
type ClientFrame struct {
	Type string `json:"type"`           // stdin, ping, resize
	Data string `json:"data,omitempty"` // input data for stdin frames
	Cols int    `json:"cols,omitempty"` // terminal columns for resize
	Rows int    `json:"rows,omitempty"` // terminal rows for resize
}
