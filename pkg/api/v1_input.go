package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func (s *Server) handleV1NativeInput(c *gin.Context) {
	const op = "send_v1_native_input"
	var request v1NativeInputRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "decode native input", err))
		return
	}
	if request.Kind != nativeprotocol.InputPrompt && request.Kind != nativeprotocol.InputSteer {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "kind must be prompt or steer", nil))
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "input text is required", nil))
		return
	}
	if request.IdempotencyKey == "" {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "idempotency_key is required", nil))
		return
	}
	alreadyDispatched, err := s.sendSessionInput(c.Request.Context(), c.Param("id"), request.IdempotencyKey, request.Kind, request.Text)
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if alreadyDispatched {
		c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true, "idempotent": true}})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
}

// SendSessionInput lets the named-chat layer use the same durable control path
// as the public v1 endpoint without writing raw provider stdin.
func (s *Server) SendSessionInput(ctx context.Context, sessionID, idempotencyKey, kind, text string) error {
	inputKind := nativeprotocol.InputKind(kind)
	if inputKind != nativeprotocol.InputPrompt && inputKind != nativeprotocol.InputSteer {
		return durable.NewError(durable.CodeInvalidArgument, "send_session_input", "kind must be prompt or steer", nil)
	}
	if idempotencyKey == "" || strings.TrimSpace(text) == "" {
		return durable.NewError(durable.CodeInvalidArgument, "send_session_input", "idempotency key and input text are required", nil)
	}
	_, err := s.sendSessionInput(ctx, sessionID, idempotencyKey, inputKind, text)
	return err
}

// turnClosingError marks a prompt that arrived after a turn ended but before
// its provider exited; the caller waits for that exit and wakes a new generation.
type turnClosingError struct{ active *activeNativeSession }

func (turnClosingError) Error() string { return "provider turn ended and the provider is exiting" }

func (s *Server) sendSessionInput(ctx context.Context, sessionID, idempotencyKey string, kind nativeprotocol.InputKind, text string) (bool, error) {
	repeated, err := s.sendSessionInputLocked(ctx, sessionID, idempotencyKey, kind, text)
	var closing turnClosingError
	if !errors.As(err, &closing) {
		return repeated, err
	}
	if err := s.waitForTurnClose(ctx, sessionID, closing.active); err != nil {
		return false, err
	}
	repeated, err = s.sendSessionInputLocked(ctx, sessionID, idempotencyKey, kind, text)
	if errors.As(err, &closing) {
		return false, errTurnStillClosing()
	}
	return repeated, err
}

func errTurnStillClosing() error {
	return durable.NewError(durable.CodeInvalidState, "send_v1_native_input", "previous provider turn is still closing; retry", nil)
}

// waitForTurnClose waits, without holding the session lock, until the exiting
// generation is detached and durably finalized.
func (s *Server) waitForTurnClose(ctx context.Context, sessionID string, closing *activeNativeSession) error {
	wait := s.turnCloseWait
	if wait <= 0 {
		wait = 15 * time.Second
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if current := s.nativeSession(sessionID); current == nil || current != closing {
			if stored, err := s.durableStore.GetSession(ctx, sessionID); err == nil && stored.State.Terminal() {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errTurnStillClosing()
		case <-tick.C:
		}
	}
}

func (s *Server) sendSessionInputLocked(ctx context.Context, sessionID, idempotencyKey string, kind nativeprotocol.InputKind, text string) (bool, error) {
	const op = "send_v1_native_input"
	if !s.beginAdmission() {
		return false, durable.NewError(durable.CodeInvalidState, op, errAdmissionClosed.Error(), nil)
	}
	defer s.endAdmission()
	unlock := s.sessionLocks.lock(sessionID)
	defer unlock()
	if s.durableStore == nil || s.eventBroker == nil {
		return false, durable.NewError(durable.CodeIndeterminate, op, "durable session services unavailable", nil)
	}
	// Retry lookup precedes lifecycle checks, so retries remain read-only after exit.
	controlProbe, err := s.nativeControlParamsContext(ctx, sessionID, idempotencyKey, string(kind), map[string]any{"text": text})
	if err != nil {
		return false, err
	}
	repeated, err := s.probeSessionControl(ctx, controlProbe)
	if err != nil || repeated {
		return repeated, err
	}
	active := s.nativeSession(sessionID)
	if active == nil {
		if kind == nativeprotocol.InputPrompt {
			_, repeated, err := s.resumeSessionLocked(ctx, sessionID, idempotencyKey, v1ResumeRequest{Prompt: text}, true)
			return repeated, err
		}
		return false, durable.NewError(durable.CodeInvalidState, op, "native transport is unavailable", nil)
	}
	active.controlMu.Lock()
	defer active.controlMu.Unlock()
	control := controlProbe
	if active.isClosing() {
		if kind == nativeprotocol.InputPrompt {
			return false, turnClosingError{active: active}
		}
		return false, durable.NewError(durable.CodeInvalidState, op, "provider turn has ended; use prompt to continue", nil)
	}
	if kind == nativeprotocol.InputPrompt {
		active.mu.Lock()
		idle := active.idle && !active.terminalClaimed
		active.mu.Unlock()
		if !idle {
			return false, durable.NewError(durable.CodeInvalidState, op, "provider turn is still active; use steer instead of prompt", nil)
		}
	}
	leaseClaimed := false
	if active.lease != nil {
		if err := active.lease.BeginInput(string(kind)); err != nil {
			return false, durable.NewError(durable.CodeInvalidState, op, err.Error(), nil)
		}
		leaseClaimed = kind == nativeprotocol.InputPrompt
	}
	begin, err := s.eventBroker.BeginControl(ctx, control)
	if err != nil || begin.AlreadyDispatched {
		if leaseClaimed {
			active.lease.AbortPrompt()
		}
		return begin.AlreadyDispatched, err
	}
	if kind == nativeprotocol.InputPrompt {
		active.mu.Lock()
		active.idle = false
		active.mu.Unlock()
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := active.transport.Send(sendCtx, nativeprotocol.Input{Kind: kind, Text: text}); err != nil {
		if leaseClaimed {
			active.lease.AbortPrompt()
		}
		return false, durable.NewError(durable.CodeIndeterminate, op, "send provider input", err)
	}
	if _, err := s.eventBroker.CompleteControl(sendCtx, control); err != nil {
		return false, durable.NewError(durable.CodeIndeterminate, op, "provider input may have been sent without durable dispatch proof", err)
	}
	return false, nil
}
