package nativeprotocol

import (
	"context"
	"encoding/json"
)

// ClaudeSteerBoundary recognizes native completion races and explicit streaming
// aborts. It never treats authentication/API errors as an interrupted turn.
func ClaudeSteerBoundary(raw []byte) bool {
	var result struct {
		Type           string `json:"type"`
		Subtype        string `json:"subtype"`
		IsError        bool   `json:"is_error"`
		TerminalReason string `json:"terminal_reason"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Type != "result" {
		return false
	}
	return (!result.IsError && result.Subtype == "success") || (result.IsError && result.Subtype == "error_during_execution" && result.TerminalReason == "aborted_streaming")
}

// A complete interrupt+replacement write must succeed before stdout may claim
// its superseded boundary. Failed or partial writes never authorize a keepalive.
func (transport *StreamTransport) beginSteerWrite(kind InputKind) func(bool) {
	done := make(chan struct{})
	transport.mu.Lock()
	transport.steerWriteDone = done
	transport.mu.Unlock()
	return func(success bool) {
		transport.mu.Lock()
		if success {
			if kind == InputSteer && transport.claudeTurnActive {
				transport.pendingSteerBoundaries++
			}
			transport.claudeTurnActive = true
		}
		transport.steerWriteDone = nil
		close(done)
		transport.mu.Unlock()
	}
}
func (transport *StreamTransport) supersededBySteer(raw []byte, eventType string) bool {
	if transport.adapter.Provider() != ProviderClaude || (eventType != "turn.completed" && eventType != "turn.failed") {
		return false
	}
	transport.mu.RLock()
	writing := transport.steerWriteDone
	transport.mu.RUnlock()
	if writing != nil {
		select {
		case <-writing:
		case <-transport.done:
			return false
		}
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	superseded := transport.pendingSteerBoundaries > 0 && ClaudeSteerBoundary(raw)
	if transport.pendingSteerBoundaries > 0 {
		transport.pendingSteerBoundaries--
	}
	transport.claudeTurnActive = superseded
	return superseded
}
func (transport *StreamTransport) writeSteerMessages(ctx context.Context, op string, messages [][]byte, kind InputKind) (err error) {
	transport.writeMu.Lock()
	defer transport.writeMu.Unlock()
	if transport.adapter.Provider() == ProviderClaude && (kind == InputPrompt || kind == InputSteer) {
		finish := transport.beginSteerWrite(kind)
		defer func() { finish(err == nil) }()
	}
	return transport.writeMessagesLocked(ctx, op, messages)
}
