package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	Subprotocols:    []string{"agentd.v1"},
	CheckOrigin:     sameOriginWebSocket,
}

func sameOriginWebSocket(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return parsed.Host == request.Host
}

func (s *Server) handleV1EventStream(c *gin.Context) {
	if s.eventBroker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": apiErrorEnvelope{Code: durable.CodeIndeterminate, Message: "durable event broker unavailable"}})
		return
	}
	after, ok := parseNonnegativeQuery(c, "after_sequence", 0)
	if !ok {
		return
	}
	streamCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	subscription, err := s.eventBroker.Subscribe(streamCtx, c.Param("id"), after, 256)
	if err != nil {
		writeDurableError(c, err)
		return
	}
	defer subscription.Close()
	progress, unsubscribeProgress := s.progress.subscribe(c.Param("id"))
	defer unsubscribeProgress()
	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	go cancelWhenWebSocketCloses(conn, cancel)
	if err := conn.WriteJSON(streamReadyFrame{
		FrameType: "stream.ready", SchemaVersion: eventstream.SchemaVersion,
		SessionID: c.Param("id"), AfterSequence: after,
		EarliestSequence: subscription.EarliestSequence(), ReplayThrough: subscription.ReplayUntil(),
	}); err != nil {
		return
	}
	type eventResult struct {
		event durable.Event
		err   error
	}
	events := make(chan eventResult, 1)
	go func() {
		for {
			event, nextErr := subscription.Next(streamCtx)
			select {
			case events <- eventResult{event: event, err: nextErr}:
			case <-streamCtx.Done():
				return
			}
			if nextErr != nil {
				return
			}
		}
	}()
	for {
		var payload any
		select {
		case update := <-progress:
			payload = update
		case result := <-events:
			if result.err != nil {
				if !errors.Is(result.err, context.Canceled) && !errors.Is(result.err, io.EOF) {
					_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
					_ = conn.WriteJSON(gin.H{"frame_type": "error", "error": durableErrorEnvelope(result.err)})
				}
				return
			}
			payload = wireEvent(result.event)
		case <-streamCtx.Done():
			return
		}
		if err := conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return
		}
		if err := conn.WriteJSON(payload); err != nil {
			return
		}
	}
}

func cancelWhenWebSocketCloses(conn *websocket.Conn, cancel context.CancelFunc) {
	defer cancel()
	for {
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}

func durableErrorEnvelope(err error) apiErrorEnvelope {
	var storeErr *durable.Error
	if errors.As(err, &storeErr) {
		return apiErrorEnvelope{Code: storeErr.Code, Message: storeErr.Message}
	}
	return apiErrorEnvelope{Code: durable.CodeIndeterminate, Message: "event stream failed"}
}
