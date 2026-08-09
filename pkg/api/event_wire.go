package api

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

type eventEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	EventID       string          `json:"event_id"`
	SessionID     string          `json:"session_id"`
	Generation    int64           `json:"generation"`
	Sequence      int64           `json:"sequence"`
	Timestamp     time.Time       `json:"timestamp"`
	Type          string          `json:"type"`
	Stream        durable.Stream  `json:"stream"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	RawBase64     string          `json:"raw_base64"`
	RawSHA256     string          `json:"raw_sha256"`
}

type eventPageEnvelope struct {
	Events           []eventEnvelope `json:"events"`
	EarliestSequence int64           `json:"earliest_sequence"`
	LastSequence     int64           `json:"last_sequence"`
	HasMore          bool            `json:"has_more"`
}

type streamReadyFrame struct {
	FrameType        string `json:"frame_type"`
	SchemaVersion    string `json:"schema_version"`
	SessionID        string `json:"session_id"`
	AfterSequence    int64  `json:"after_sequence"`
	EarliestSequence int64  `json:"earliest_sequence"`
	ReplayThrough    int64  `json:"replay_through"`
}

func wireEvent(event durable.Event) eventEnvelope {
	return eventEnvelope{
		SchemaVersion: event.SchemaVersion,
		EventID:       event.EventID,
		SessionID:     event.SessionID,
		Generation:    event.Generation,
		Sequence:      event.Sequence,
		Timestamp:     event.Timestamp,
		Type:          event.Type,
		Stream:        event.Stream,
		Payload:       append(json.RawMessage(nil), event.Payload...),
		RawBase64:     base64.StdEncoding.EncodeToString(event.Raw),
		RawSHA256:     event.RawSHA256,
	}
}
