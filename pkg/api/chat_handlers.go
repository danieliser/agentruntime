package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/chat"
	"github.com/danieliser/agentruntime/pkg/durable"
)

// chatConfigFromAPI converts the API config type to the internal chat config.
func chatConfigFromAPI(c apischema.ChatAPIConfig) chat.ChatConfig {
	return chat.ChatConfig{
		Agent:        c.Agent,
		Runtime:      c.Runtime,
		Model:        c.Model,
		Effort:       c.Effort,
		Fast:         c.Fast,
		Context:      c.Context,
		MCPServers:   c.MCPServers,
		AutoDiscover: c.AutoDiscover,
		WorkDir:      c.WorkDir,
		Mounts:       c.Mounts,
		Env:          c.Env,
		IdleTimeout:  c.IdleTimeout,
		MaxTurns:     c.MaxTurns,
		Claude:       c.Claude,
		Codex:        c.Codex,
	}
}

// chatConfigToAPI converts the internal chat config to the API type.
func chatConfigToAPI(c chat.ChatConfig) apischema.ChatAPIConfig {
	return apischema.ChatAPIConfig{
		Agent:        c.Agent,
		Runtime:      c.Runtime,
		Model:        c.Model,
		Effort:       c.Effort,
		Fast:         c.Fast,
		Context:      c.Context,
		MCPServers:   c.MCPServers,
		AutoDiscover: c.AutoDiscover,
		WorkDir:      c.WorkDir,
		Mounts:       c.Mounts,
		Env:          c.Env,
		IdleTimeout:  c.IdleTimeout,
		MaxTurns:     c.MaxTurns,
		Claude:       c.Claude,
		Codex:        c.Codex,
	}
}

// chatRecordToResponse builds a ChatResponse from a ChatRecord.
func chatRecordToResponse(rec *chat.ChatRecord, c *gin.Context) apischema.ChatResponse {
	resp := apischema.ChatResponse{
		Name:             rec.Name,
		Config:           chatConfigToAPI(rec.Config),
		State:            string(rec.State),
		VolumeName:       rec.VolumeName,
		CurrentSession:   rec.CurrentSession,
		SessionChain:     rec.SessionChain,
		ClaudeSessionIDs: rec.ClaudeSessionIDs,
		CreatedAt:        rec.CreatedAt,
		UpdatedAt:        rec.UpdatedAt,
		LastActiveAt:     rec.LastActiveAt,
	}
	if rec.State == chat.ChatStateRunning && rec.CurrentSession != "" {
		resp.WSURL = sessionEventStreamURL(c, rec.CurrentSession)
	}
	return resp
}

// handleCreateChat handles POST /chats.
func (s *Server) handleCreateChat(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "chat subsystem not initialized"})
		return
	}

	var req apischema.CreateChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Config.Agent == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "config.agent is required"})
		return
	}

	cfg := chatConfigFromAPI(req.Config)
	rec, err := s.chatManager.CreateChat(req.Name, cfg)
	if err != nil {
		if errors.Is(err, chat.ErrAlreadyExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "chat already exists"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, chatRecordToResponse(rec, c))
}

// handleListChats handles GET /chats.
func (s *Server) handleListChats(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusOK, []apischema.ChatSummary{})
		return
	}

	records, err := s.chatManager.ListChats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	summaries := make([]apischema.ChatSummary, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, apischema.ChatSummary{
			Name:         rec.Name,
			State:        string(rec.State),
			Agent:        rec.Config.Agent,
			Runtime:      rec.Config.Runtime,
			SessionCount: len(rec.SessionChain),
			CreatedAt:    rec.CreatedAt,
			LastActiveAt: rec.LastActiveAt,
		})
	}
	c.JSON(http.StatusOK, summaries)
}

// handleGetChat handles GET /chats/:name.
func (s *Server) handleGetChat(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
		return
	}

	name := c.Param("name")
	rec, err := s.chatManager.GetChat(name)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, chatRecordToResponse(rec, c))
}

// handleSendMessage handles POST /chats/:name/messages.
func (s *Server) handleSendMessage(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "chat subsystem not initialized"})
		return
	}

	name := c.Param("name")
	var req apischema.SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return
	}

	result, err := s.chatManager.SendMessage(name, req.Message)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
			return
		}
		if errors.Is(err, chat.ErrChatBusy) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":          "chat is busy",
				"retry_after_ms": 5000,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Reload record for current state.
	rec, _ := s.chatManager.GetChat(name)
	state := "running"
	if rec != nil {
		state = string(rec.State)
	}

	c.JSON(http.StatusAccepted, apischema.SendMessageResponse{
		SessionID: result.SessionID,
		State:     state,
		Queued:    result.Queued,
		Spawned:   result.Spawned,
		WSURL:     sessionEventStreamURL(c, result.SessionID),
	})
}

// messageEventTypeList are the event types included in chat message history.
var messageEventTypeList = []string{
	"agent_message",
	"tool_use",
	"tool_result",
	"result",
	"error",
}

// handleGetChatMessages handles GET /chats/:name/messages.
func (s *Server) handleGetChatMessages(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
		return
	}

	name := c.Param("name")
	rec, err := s.chatManager.GetChat(name)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	limit := 100
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsed, parseErr := strconv.Atoi(limitStr); parseErr == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 500 {
		limit = 500
	}

	var beforeCursor int64 = -1
	if beforeStr := c.Query("before"); beforeStr != "" {
		if parsed, parseErr := strconv.ParseInt(beforeStr, 10, 64); parseErr == nil {
			beforeCursor = parsed
		}
	}

	messages, hasMore, err := s.readChatMessages(c.Request.Context(), rec.SessionChain, limit, beforeCursor)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var beforeOut int64
	if len(messages) > 0 {
		beforeOut = messages[0].Offset
	}

	c.JSON(http.StatusOK, apischema.ChatMessagesResponse{
		Messages: messages,
		Total:    len(messages),
		HasMore:  hasMore,
		Before:   beforeOut,
	})
}

func (s *Server) readChatMessages(ctx context.Context, chain []string, limit int, before int64) ([]apischema.ChatMessageEntry, bool, error) {
	entries := make([]apischema.ChatMessageEntry, 0)
	for chainIndex, sessionID := range chain {
		sessionEntries, err := s.readChatSessionMessages(ctx, sessionID, chainIndex)
		if err != nil {
			return nil, false, err
		}
		for _, entry := range sessionEntries {
			if before > 0 && entry.Offset >= before {
				continue
			}
			entries = append(entries, entry)
		}
	}
	if limit > 0 && len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

func (s *Server) readChatSessionMessages(ctx context.Context, sessionID string, chainIndex int) ([]apischema.ChatMessageEntry, error) {
	if s.durableStore != nil {
		if _, err := s.durableStore.GetSession(ctx, sessionID); err == nil {
			return s.readDurableChatSessionMessages(ctx, sessionID, chainIndex)
		} else if !durable.IsCode(err, durable.CodeNotFound) {
			return nil, err
		}
	}
	legacy, _, err := chat.NewLogReader(s.logDir).ReadMessages([]string{sessionID}, 0, 0, messageEventTypeList)
	if err != nil {
		return nil, err
	}
	for index := range legacy {
		legacy[index].Offset += int64(chainIndex) * 1_000_000_000
		legacy[index].Source = "legacy_ndjson_unverified"
	}
	return legacy, nil
}

func (s *Server) readDurableChatSessionMessages(ctx context.Context, sessionID string, chainIndex int) ([]apischema.ChatMessageEntry, error) {
	entries := make([]apischema.ChatMessageEntry, 0)
	cursor := int64(0)
	for {
		page, err := s.durableStore.ListEvents(ctx, durable.EventQuery{SessionID: sessionID, AfterSequence: cursor, Limit: 1000})
		if err != nil {
			return nil, err
		}
		for _, event := range page.Events {
			cursor = event.Sequence
			typeName := ""
			switch event.Type {
			case "content.delta":
				typeName = "agent_message"
			case "tool.call":
				typeName = "tool_use"
			case "tool.result":
				typeName = "tool_result"
			case "usage":
				typeName = "result"
			case "runtime.stderr", "error.protocol":
				typeName = "error"
			default:
				continue
			}
			entries = append(entries, apischema.ChatMessageEntry{
				SessionID: sessionID, Type: typeName, Data: append([]byte(nil), event.Payload...),
				Offset:    int64(chainIndex)*1_000_000_000 + event.Sequence,
				Timestamp: event.Timestamp, Source: "durable_v1",
			})
		}
		if !page.HasMore {
			break
		}
		if len(page.Events) == 0 {
			return nil, durable.NewError(durable.CodeIndeterminate, "read_chat_history", "event page did not advance", nil)
		}
	}
	return entries, nil
}

// handleChatAttach handles POST /chats/:name/attach.
// Spawns an interactive session (no prompt) through the chat manager.
func (s *Server) handleChatAttach(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "chat subsystem not initialized"})
		return
	}

	name := c.Param("name")
	result, err := s.chatManager.AttachSession(name)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, apischema.SendMessageResponse{
		SessionID: result.SessionID,
		State:     "running",
		Spawned:   result.Spawned,
		WSURL:     sessionEventStreamURL(c, result.SessionID),
	})
}

// handleUpdateChatConfig handles PATCH /chats/:name/config.
func (s *Server) handleUpdateChatConfig(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "chat subsystem not initialized"})
		return
	}

	name := c.Param("name")
	rec, err := s.chatManager.GetChat(name)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if rec.State != chat.ChatStateIdle && rec.State != chat.ChatStateCreated {
		c.JSON(http.StatusConflict, gin.H{"error": "config can only be updated when chat is idle"})
		return
	}

	var req apischema.UpdateChatConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Deep merge: only update fields that are provided.
	if req.Config.Agent != "" {
		rec.Config.Agent = req.Config.Agent
	}
	if req.Config.Runtime != "" {
		rec.Config.Runtime = req.Config.Runtime
	}
	if req.Config.Model != "" {
		rec.Config.Model = req.Config.Model
	}
	if req.Config.Effort != "" {
		rec.Config.Effort = req.Config.Effort
	}
	if req.Config.Fast {
		rec.Config.Fast = true
	}
	if req.Config.WorkDir != "" {
		rec.Config.WorkDir = req.Config.WorkDir
	}
	if len(req.Config.Mounts) > 0 {
		rec.Config.Mounts = req.Config.Mounts
	}
	if req.Config.IdleTimeout != "" {
		rec.Config.IdleTimeout = req.Config.IdleTimeout
	}
	rec.UpdatedAt = time.Now()

	if err := s.chatRegistry.Save(rec); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, chatRecordToResponse(rec, c))
}

// handleDeleteChat handles DELETE /chats/:name.
func (s *Server) handleDeleteChat(c *gin.Context) {
	if s.chatManager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
		return
	}

	name := c.Param("name")
	removeVolume := c.Query("remove_volume") == "true"

	err := s.chatManager.DeleteChat(name, removeVolume)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"name": name, "deleted": true})
}
