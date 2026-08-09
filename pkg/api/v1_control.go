package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

type v1NativeInputRequest struct {
	Kind nativeprotocol.InputKind `json:"kind"`
	Text string                   `json:"text"`
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
	transport := s.nativeTransport(c.Param("id"))
	if transport == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "session has no active native transport", nil))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := transport.Send(ctx, nativeprotocol.Input{Kind: request.Kind, Text: request.Text}); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "send provider input", err))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
}

func (s *Server) handleV1NativeInterrupt(c *gin.Context) {
	const op = "interrupt_v1_native_session"
	transport := s.nativeTransport(c.Param("id"))
	if transport == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "session has no active native transport", nil))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := transport.Interrupt(ctx); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "send provider interrupt", err))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
}
