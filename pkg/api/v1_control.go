package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

type v1NativeInputRequest struct {
	IdempotencyKey string                   `json:"idempotency_key"`
	Kind           nativeprotocol.InputKind `json:"kind"`
	Text           string                   `json:"text"`
}

type v1InterruptRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}

type activeNativeSession struct {
	transport nativeprotocol.Transport

	mu                 sync.Mutex
	terminalRequested  durable.SessionState
	terminalOutcome    durable.SessionState
	terminalClaimed    bool
	terminalSettled    chan struct{}
	terminalSettleOnce sync.Once
	finished           chan struct{}
	finishedOnce       sync.Once
}

func newActiveNativeSession(transport nativeprotocol.Transport) *activeNativeSession {
	return &activeNativeSession{
		transport: transport, terminalSettled: make(chan struct{}), finished: make(chan struct{}),
	}
}

func (active *activeNativeSession) beginCancel() bool {
	return active.beginTerminal(durable.StateCancelled)
}

func (active *activeNativeSession) beginTimeout() bool {
	return active.beginTerminal(durable.StateTimedOut)
}

func (active *activeNativeSession) beginTerminal(state durable.SessionState) bool {
	active.mu.Lock()
	defer active.mu.Unlock()
	if active.terminalClaimed {
		return false
	}
	active.terminalClaimed = true
	active.terminalRequested = state
	return true
}

func (active *activeNativeSession) settleCancel(state durable.SessionState) {
	active.settleTerminal(state)
}

func (active *activeNativeSession) settleTimeout(state durable.SessionState) {
	active.settleTerminal(state)
}

func (active *activeNativeSession) settleTerminal(state durable.SessionState) {
	active.terminalSettleOnce.Do(func() {
		active.mu.Lock()
		active.terminalOutcome = state
		active.mu.Unlock()
		close(active.terminalSettled)
	})
}

func (active *activeNativeSession) terminalReason() string {
	active.mu.Lock()
	if !active.terminalClaimed {
		active.terminalClaimed = true
		active.mu.Unlock()
		active.markFinished()
		return ""
	}
	requested := active.terminalRequested
	active.mu.Unlock()
	if requested == "" {
		active.markFinished()
		return ""
	}
	<-active.terminalSettled
	outcome := active.terminalState()
	active.markFinished()
	if outcome == durable.StateCancelled || outcome == durable.StateTimedOut {
		return string(outcome)
	}
	return string(durable.StateIndeterminate)
}

func (active *activeNativeSession) terminalState() durable.SessionState {
	active.mu.Lock()
	defer active.mu.Unlock()
	return active.terminalOutcome
}

func (active *activeNativeSession) markFinished() {
	active.finishedOnce.Do(func() { close(active.finished) })
}

func (s *Server) setNativeTransport(sessionID string, transport nativeprotocol.Transport) *activeNativeSession {
	active := newActiveNativeSession(transport)
	s.nativeMu.Lock()
	s.native[sessionID] = active
	s.nativeMu.Unlock()
	return active
}

func (s *Server) clearNativeTransport(sessionID string, active *activeNativeSession) {
	s.nativeMu.Lock()
	if current := s.native[sessionID]; current == active {
		delete(s.native, sessionID)
	}
	s.nativeMu.Unlock()
}

func (s *Server) nativeSession(sessionID string) *activeNativeSession {
	s.nativeMu.RLock()
	defer s.nativeMu.RUnlock()
	return s.native[sessionID]
}

func (s *Server) armNativeTimeout(sessionID string, generation int64, active *activeNativeSession, timeout time.Duration, startedAt time.Time) {
	if active == nil {
		return
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	wait := time.Until(startedAt.Add(timeout))
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	go func() {
		defer timer.Stop()
		select {
		case <-active.finished:
			return
		case <-timer.C:
			s.expireNativeSession(sessionID, generation, active, timeout)
		}
	}()
}

func (s *Server) expireNativeSession(sessionID string, generation int64, active *activeNativeSession, timeout time.Duration) {
	if !active.beginTimeout() {
		return
	}
	controlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	control, alreadyDispatched, err := s.beginNativeControlContext(
		controlCtx, sessionID, fmt.Sprintf("timeout:%s:g%d", sessionID, generation), "timeout",
		map[string]any{"duration": timeout.String()},
	)
	cancel()
	if err != nil {
		_ = active.transport.Close()
		active.settleTimeout(durable.StateIndeterminate)
		log.Printf("[session %s] timeout intent could not be committed: %v", sessionID, err)
		return
	}
	if err := active.transport.Close(); err != nil {
		active.settleTimeout(durable.StateIndeterminate)
		log.Printf("[session %s] timeout termination could not be confirmed: %v", sessionID, err)
		return
	}
	if !alreadyDispatched {
		dispatchCtx, dispatchCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = s.eventBroker.CompleteControl(dispatchCtx, control)
		dispatchCancel()
		if err != nil {
			active.settleTimeout(durable.StateIndeterminate)
			log.Printf("[session %s] process stopped without durable timeout dispatch proof: %v", sessionID, err)
			return
		}
	}
	active.settleTimeout(durable.StateTimedOut)
}

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

func (s *Server) sendSessionInput(ctx context.Context, sessionID, idempotencyKey string, kind nativeprotocol.InputKind, text string) (bool, error) {
	const op = "send_v1_native_input"
	control, alreadyDispatched, err := s.beginNativeControlContext(ctx, sessionID, idempotencyKey, string(kind), map[string]any{"text": text})
	if err != nil || alreadyDispatched {
		return alreadyDispatched, err
	}
	active := s.nativeSession(sessionID)
	if active == nil {
		return false, durable.NewError(durable.CodeIndeterminate, op, "control intent is durable but the native transport is unavailable", nil)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := active.transport.Send(sendCtx, nativeprotocol.Input{Kind: kind, Text: text}); err != nil {
		return false, durable.NewError(durable.CodeIndeterminate, op, "send provider input", err)
	}
	if _, err := s.eventBroker.CompleteControl(sendCtx, control); err != nil {
		return false, durable.NewError(durable.CodeIndeterminate, op, "provider input may have been sent without durable dispatch proof", err)
	}
	return false, nil
}

func (s *Server) handleV1NativeInterrupt(c *gin.Context) {
	const op = "interrupt_v1_native_session"
	var request v1InterruptRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "decode interrupt request", err))
		return
	}
	if request.IdempotencyKey == "" {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "idempotency_key is required", nil))
		return
	}
	control, alreadyDispatched, err := s.beginNativeControl(c, request.IdempotencyKey, "interrupt", map[string]any{})
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if alreadyDispatched {
		c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true, "idempotent": true}})
		return
	}
	active := s.nativeSession(c.Param("id"))
	if active == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "control intent is durable but the native transport is unavailable", nil))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := active.transport.Interrupt(ctx); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "send provider interrupt", err))
		return
	}
	if _, err := s.eventBroker.CompleteControl(ctx, control); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "provider interrupt may have been sent without durable dispatch proof", err))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
}

func (s *Server) handleV1NativeCancel(c *gin.Context) {
	const op = "cancel_v1_native_session"
	var request v1InterruptRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "decode cancel request", err))
		return
	}
	if request.IdempotencyKey == "" {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "idempotency_key is required", nil))
		return
	}
	control, alreadyDispatched, err := s.beginNativeControl(c, request.IdempotencyKey, "cancel", map[string]any{})
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if alreadyDispatched {
		c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true, "idempotent": true}})
		return
	}
	active := s.nativeSession(c.Param("id"))
	if active == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "cancel intent is durable but the native transport is unavailable", nil))
		return
	}
	if !active.beginCancel() {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "cancel intent is durable but process exit already crossed the terminal boundary", nil))
		return
	}
	if err := active.transport.Close(); err != nil {
		active.settleCancel(durable.StateIndeterminate)
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "native process termination could not be confirmed", err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.eventBroker.CompleteControl(ctx, control); err != nil {
		active.settleCancel(durable.StateIndeterminate)
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "native process stopped without durable cancel dispatch proof", err))
		return
	}
	active.settleCancel(durable.StateCancelled)
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
}

func (s *Server) beginNativeControl(c *gin.Context, idempotencyKey, kind string, command any) (eventstream.ControlParams, bool, error) {
	return s.beginNativeControlContext(c.Request.Context(), c.Param("id"), idempotencyKey, kind, command)
}

func (s *Server) beginNativeControlContext(ctx context.Context, sessionID, idempotencyKey, kind string, command any) (eventstream.ControlParams, bool, error) {
	const op = "begin_v1_native_control"
	if s.durableStore == nil || s.eventBroker == nil {
		return eventstream.ControlParams{}, false, durable.NewError(durable.CodeIndeterminate, op, "durable session services unavailable", nil)
	}
	stored, err := s.durableStore.GetSession(ctx, sessionID)
	if err != nil {
		return eventstream.ControlParams{}, false, err
	}
	if stored.ActiveGeneration < 1 {
		return eventstream.ControlParams{}, false, durable.NewError(durable.CodeInvalidState, op, "session has no runtime generation", nil)
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return eventstream.ControlParams{}, false, durable.NewError(durable.CodeInvalidArgument, op, "encode control command", err)
	}
	params := eventstream.ControlParams{
		SessionID: stored.ID, Generation: stored.ActiveGeneration, IdempotencyKey: idempotencyKey,
		Timestamp: time.Now().UTC(), Kind: kind, Payload: payload,
	}
	begin, err := s.eventBroker.BeginControl(ctx, params)
	if err != nil {
		return eventstream.ControlParams{}, false, err
	}
	return params, begin.AlreadyDispatched, nil
}
