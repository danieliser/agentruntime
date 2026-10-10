package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"time"
)

type v1ApprovalDecision struct {
	RequestID      json.RawMessage `json:"request_id"`
	Decision       string          `json:"decision"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (s *Server) handleV1PendingApprovals(c *gin.Context) {
	active := s.nativeSession(c.Param("id"))
	if active == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, "pending_approvals", "native transport unavailable", nil))
		return
	}
	transport, ok := active.transport.(nativeprotocol.ApprovalTransport)
	if !ok {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, "pending_approvals", "approval transport unavailable", nil))
		return
	}
	pending := make([]nativeprotocol.PendingApproval, 0)
	for _, p := range transport.PendingApprovals() {
		params, err := s.nativeControlParamsContext(c.Request.Context(), c.Param("id"), approvalControlKey(p.RequestID), "approval", struct {
			RequestID json.RawMessage `json:"request_id"`
			Decision  string          `json:"decision"`
		}{p.RequestID, "accept"})
		if err != nil {
			writeDurableError(c, err)
			return
		}
		probe, err := s.eventBroker.ProbeControl(c.Request.Context(), params)
		if durable.IsCode(err, durable.CodeImmutableConflict) || durable.IsCode(err, durable.CodeIndeterminate) || probe.AlreadyDispatched {
			continue
		}
		if err != nil {
			writeDurableError(c, err)
			return
		}
		pending = append(pending, p)
	}

	c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "approvals": pending}})
}
func (s *Server) handleV1ApprovalDecision(c *gin.Context) {
	const op = "decide_v1_approval"
	var req v1ApprovalDecision
	if err := c.ShouldBindJSON(&req); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "invalid approval body", err))
		return
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 256 || !nativeprotocol.ValidApprovalRequestID(req.RequestID) || len(req.RequestID) > 512 || (req.Decision != "accept" && req.Decision != "decline" && req.Decision != "cancel") {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "exact request_id, decision and idempotency_key required", nil))
		return
	}
	active := s.nativeSession(c.Param("id"))
	if active == nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "native transport unavailable", nil))
		return
	}
	transport, ok := active.transport.(nativeprotocol.ApprovalTransport)
	if !ok {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "approval transport unavailable", nil))
		return
	}
	active.controlMu.Lock()
	defer active.controlMu.Unlock()
	control, err := s.nativeControlParamsContext(c.Request.Context(), c.Param("id"), approvalControlKey(req.RequestID), "approval", struct {
		RequestID json.RawMessage `json:"request_id"`
		Decision  string          `json:"decision"`
	}{req.RequestID, req.Decision})
	if err != nil {
		writeDurableError(c, err)
		return
	}
	probe, err := s.eventBroker.ProbeControl(c.Request.Context(), control)
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if probe.AlreadyDispatched {
		c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true, "idempotent": true}})
		return
	}
	pending := false
	for _, p := range transport.PendingApprovals() {
		if string(p.RequestID) == string(req.RequestID) && time.Now().Before(p.ExpiresAt) {
			pending = true
			if req.Decision == "accept" && !p.CanAccept {
				writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "approval input unsupported", nil))
				return
			}
			break
		}
	}
	if !pending {
		writeDurableError(c, durable.NewError(durable.CodeInvalidState, op, "request is not pending in this session", nil))
		return
	}
	begin, err := s.eventBroker.BeginControl(c.Request.Context(), control)
	if err != nil {
		writeDurableError(c, err)
		return
	}
	if begin.AlreadyDispatched {
		c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true, "idempotent": true}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := transport.DecideApproval(ctx, req.RequestID, req.Decision); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "approval delivery unknown; do not replay", err))
		return
	}
	if _, err := s.eventBroker.CompleteControl(ctx, control); err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "approval sent without durable delivery proof", err))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"api_version": "v1", "data": gin.H{"session_id": c.Param("id"), "accepted": true}})
}

func approvalControlKey(id json.RawMessage) string {
	return fmt.Sprintf("approval:%x", sha256.Sum256(id))
}
