package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

func (s *Server) setNativeTransport(sessionID string, transport nativeprotocol.Transport) {
	s.nativeMu.Lock()
	s.native[sessionID] = transport
	s.nativeMu.Unlock()
}

func (s *Server) clearNativeTransport(sessionID string, transport nativeprotocol.Transport) {
	s.nativeMu.Lock()
	if current := s.native[sessionID]; current == transport {
		delete(s.native, sessionID)
	}
	s.nativeMu.Unlock()
}

func (s *Server) nativeTransport(sessionID string) nativeprotocol.Transport {
	s.nativeMu.RLock()
	defer s.nativeMu.RUnlock()
	return s.native[sessionID]
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
	transport := s.nativeTransport(c.Param("id"))
	if transport == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "session has no active native transport", nil))
		return
	}
	control, alreadyDispatched, err := s.beginNativeControl(c, request.IdempotencyKey, string(request.Kind), map[string]any{"text": request.Text})
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if alreadyDispatched {
		c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true, "idempotent": true}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := transport.Send(ctx, nativeprotocol.Input{Kind: request.Kind, Text: request.Text}); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "send provider input", err))
		return
	}
	if _, err := s.eventBroker.CompleteControl(ctx, control); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "provider input may have been sent without durable dispatch proof", err))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
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
	transport := s.nativeTransport(c.Param("id"))
	if transport == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "session has no active native transport", nil))
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := transport.Interrupt(ctx); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "send provider interrupt", err))
		return
	}
	if _, err := s.eventBroker.CompleteControl(ctx, control); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "provider interrupt may have been sent without durable dispatch proof", err))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
}

func (s *Server) beginNativeControl(c *gin.Context, idempotencyKey, kind string, command any) (eventstream.ControlParams, bool, error) {
	const op = "begin_v1_native_control"
	if s.durableStore == nil || s.eventBroker == nil {
		return eventstream.ControlParams{}, false, durable.NewError(durable.CodeIndeterminate, op, "durable session services unavailable", nil)
	}
	stored, err := s.durableStore.GetSession(c.Request.Context(), c.Param("id"))
	if err != nil {
		return eventstream.ControlParams{}, false, err
	}
	if stored.State.Terminal() || stored.ActiveGeneration < 1 {
		return eventstream.ControlParams{}, false, durable.NewError(durable.CodeInvalidState, op, "session is not active", nil)
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return eventstream.ControlParams{}, false, durable.NewError(durable.CodeInvalidArgument, op, "encode control command", err)
	}
	params := eventstream.ControlParams{
		SessionID: stored.ID, Generation: stored.ActiveGeneration, IdempotencyKey: idempotencyKey,
		Timestamp: time.Now().UTC(), Kind: kind, Payload: payload,
	}
	begin, err := s.eventBroker.BeginControl(c.Request.Context(), params)
	if err != nil {
		return eventstream.ControlParams{}, false, err
	}
	return params, begin.AlreadyDispatched, nil
}
