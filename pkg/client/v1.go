package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/danieliser/agentruntime/pkg/api"
)

// DurableSession is the public v1 logical-session view.
type DurableSession struct {
	SessionID      string `json:"session_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Agent          string `json:"agent"`
	Runtime        string `json:"runtime"`
	State          string `json:"state"`
	Generation     int64  `json:"generation"`
	LastSequence   int64  `json:"last_sequence"`
	EventsURL      string `json:"events_url"`
	EventStreamURL string `json:"event_stream_url"`
}

type ReplayCapabilities struct {
	SequenceCursor     bool `json:"sequence_cursor"`
	StoredThenLive     bool `json:"stored_then_live"`
	RestartPersistence bool `json:"restart_persistence"`
}

// Capabilities is the v1 compatibility handshake a caller checks before
// submitting paid work.
type Capabilities struct {
	AgentDVersion        string             `json:"agentd_version"`
	APIVersions          []string           `json:"api_versions"`
	EventSchemaVersions  []string           `json:"event_schema_versions"`
	NativeProviders      []string           `json:"native_providers"`
	Runtimes             []string           `json:"runtimes"`
	Replay               ReplayCapabilities `json:"replay"`
	DockerReconstruction bool               `json:"docker_reconstruction"`
	PluginAPIVersions    []string           `json:"plugin_api_versions"`
}

// Event is one immutable v1 AgentD event with decoded exact raw bytes.
type Event struct {
	SchemaVersion string          `json:"schema_version"`
	EventID       string          `json:"event_id"`
	SessionID     string          `json:"session_id"`
	Generation    int64           `json:"generation"`
	Sequence      int64           `json:"sequence"`
	Timestamp     time.Time       `json:"timestamp"`
	Type          string          `json:"type"`
	Stream        string          `json:"stream"`
	Payload       json.RawMessage `json:"payload"`
	Raw           []byte          `json:"-"`
	RawSHA256     string          `json:"raw_sha256"`
}

// EventPage is one sequence page from the durable ledger.
type EventPage struct {
	Events           []Event `json:"events"`
	EarliestSequence int64   `json:"earliest_sequence"`
	LastSequence     int64   `json:"last_sequence"`
	HasMore          bool    `json:"has_more"`
}

type eventWire struct {
	SchemaVersion string          `json:"schema_version"`
	EventID       string          `json:"event_id"`
	SessionID     string          `json:"session_id"`
	Generation    int64           `json:"generation"`
	Sequence      int64           `json:"sequence"`
	Timestamp     time.Time       `json:"timestamp"`
	Type          string          `json:"type"`
	Stream        string          `json:"stream"`
	Payload       json.RawMessage `json:"payload"`
	RawBase64     string          `json:"raw_base64"`
	RawSHA256     string          `json:"raw_sha256"`
}

func (c *Client) GetCapabilities(ctx context.Context) (*Capabilities, error) {
	httpRequest, err := c.newRequest(ctx, http.MethodGet, "/api/v1/capabilities", nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data Capabilities `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// DispatchDurable creates or idempotently looks up one v1 logical session.
// When a one-shot caller omits a key, the canonical request derives a stable
// SHA-256 key so retrying the same request cannot launch duplicate paid work.
func (c *Client) DispatchDurable(ctx context.Context, request api.SessionRequest) (*DurableSession, error) {
	if request.IdempotencyKey == "" {
		key, err := derivedDispatchKey(request)
		if err != nil {
			return nil, err
		}
		request.IdempotencyKey = key
	}
	httpRequest, err := c.newJSONRequest(ctx, http.MethodPost, "/api/v1/sessions", request)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data DurableSession `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data.SessionID == "" {
		return nil, fmt.Errorf("decode durable session: missing session id")
	}
	return &envelope.Data, nil
}

func derivedDispatchKey(request api.SessionRequest) (string, error) {
	request.IdempotencyKey = ""
	raw, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("derive dispatch idempotency key: %w", err)
	}
	digest := sha256.Sum256(raw)
	return "dispatch:" + hex.EncodeToString(digest[:]), nil
}

// GetDurableSession returns the current v1 logical-session state.
func (c *Client) GetDurableSession(ctx context.Context, sessionID string) (*DurableSession, error) {
	httpRequest, err := c.newRequest(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(sessionID), nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data DurableSession `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// GetEvents reads events strictly after afterSequence.
func (c *Client) GetEvents(ctx context.Context, sessionID string, afterSequence int64, limit int) (*EventPage, error) {
	values := url.Values{}
	values.Set("after_sequence", strconv.FormatInt(afterSequence, 10))
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/sessions/" + url.PathEscape(sessionID) + "/events?" + values.Encode()
	httpRequest, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data struct {
			Events           []eventWire `json:"events"`
			EarliestSequence int64       `json:"earliest_sequence"`
			LastSequence     int64       `json:"last_sequence"`
			HasMore          bool        `json:"has_more"`
		} `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	page := &EventPage{
		EarliestSequence: envelope.Data.EarliestSequence,
		LastSequence:     envelope.Data.LastSequence,
		HasMore:          envelope.Data.HasMore,
		Events:           make([]Event, 0, len(envelope.Data.Events)),
	}
	for _, wire := range envelope.Data.Events {
		raw, err := base64.StdEncoding.DecodeString(wire.RawBase64)
		if err != nil {
			return nil, fmt.Errorf("decode event %s raw bytes: %w", wire.EventID, err)
		}
		page.Events = append(page.Events, Event{
			SchemaVersion: wire.SchemaVersion, EventID: wire.EventID, SessionID: wire.SessionID,
			Generation: wire.Generation, Sequence: wire.Sequence, Timestamp: wire.Timestamp,
			Type: wire.Type, Stream: wire.Stream, Payload: append(json.RawMessage(nil), wire.Payload...),
			Raw: raw, RawSHA256: wire.RawSHA256,
		})
	}
	return page, nil
}

// StreamEventRaw exposes exact provider/runtime records in durable sequence
// order. Terminal control payloads close the stream but are not mixed into the
// provider NDJSON output.
func (c *Client) StreamEventRaw(ctx context.Context, sessionID string, afterSequence int64) (io.ReadCloser, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go c.pollEventRaw(streamCtx, writer, sessionID, afterSequence)
	return &streamReadCloser{ReadCloser: reader, cancel: cancel}, nil
}

func (c *Client) pollEventRaw(ctx context.Context, writer *io.PipeWriter, sessionID string, cursor int64) {
	defer writer.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		page, err := c.GetEvents(ctx, sessionID, cursor, 1000)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		terminal := false
		for _, event := range page.Events {
			if event.Sequence != cursor+1 {
				_ = writer.CloseWithError(fmt.Errorf("event sequence gap: got %d after %d", event.Sequence, cursor))
				return
			}
			cursor = event.Sequence
			if event.Stream == "terminal" {
				terminal = true
				continue
			}
			if len(event.Raw) > 0 {
				if _, err := writer.Write(append(append([]byte(nil), event.Raw...), '\n')); err != nil {
					return
				}
			}
		}
		if terminal {
			return
		}
		if page.HasMore {
			continue
		}
		select {
		case <-ctx.Done():
			_ = writer.CloseWithError(ctx.Err())
			return
		case <-ticker.C:
		}
	}
}
