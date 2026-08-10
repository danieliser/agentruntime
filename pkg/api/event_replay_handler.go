package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func (s *Server) handleV1EventReplay(c *gin.Context) {
	if s.durableStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": apiErrorEnvelope{Code: durable.CodeIndeterminate, Message: "durable event store unavailable"}})
		return
	}
	after, ok := parseNonnegativeQuery(c, "after_sequence", 0)
	if !ok {
		return
	}
	limit, ok := parseNonnegativeQuery(c, "limit", 100)
	if !ok {
		return
	}
	if limit > 1000 {
		limit = 1000
	}
	page, err := s.durableStore.ListEvents(c.Request.Context(), durable.EventQuery{
		SessionID: c.Param("id"), AfterSequence: after, Limit: int(limit),
	})
	if err != nil {
		writeDurableError(c, err)
		return
	}
	events := make([]eventEnvelope, 0, len(page.Events))
	for _, event := range page.Events {
		events = append(events, wireEvent(event))
	}
	c.JSON(http.StatusOK, gin.H{
		"api_version": "v1",
		"data": eventPageEnvelope{
			Events: events, EarliestSequence: page.EarliestSequence,
			LastSequence: page.LastSequence, HasMore: page.HasMore,
		},
	})
}

func (s *Server) handleV1GetStructuredResult(c *gin.Context) {
	const op = "get_structured_result"
	if s.durableStore == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "durable event store unavailable", nil))
		return
	}
	stored, err := s.durableStore.GetSession(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeDurableError(c, err)
		return
	}
	after := stored.LastSequence - 3
	if after < 0 {
		after = 0
	}
	page, err := s.durableStore.ListEvents(c.Request.Context(), durable.EventQuery{
		SessionID: stored.ID, AfterSequence: after, Limit: 3,
	})
	if err != nil {
		writeDurableError(c, err)
		return
	}
	for index := len(page.Events) - 1; index >= 0; index-- {
		event := page.Events[index]
		if event.Type != "output.final" || event.Stream != durable.StreamTerminal {
			continue
		}
		c.Header("X-Content-SHA256", event.RawSHA256)
		c.Header("X-Event-ID", event.EventID)
		c.Header("X-Event-Sequence", strconv.FormatInt(event.Sequence, 10))
		c.Data(http.StatusOK, "application/json", event.Raw)
		return
	}
	writeDurableError(c, durable.NewError(durable.CodeNotFound, op, "structured result is not available", nil))
}

func parseNonnegativeQuery(c *gin.Context, name string, fallback int64) (int64, bool) {
	raw := c.Query(name)
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": apiErrorEnvelope{Code: durable.CodeInvalidArgument, Message: name + " must be a nonnegative integer"}})
		return 0, false
	}
	return value, true
}
