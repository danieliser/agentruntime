// Package observer defines AgentD's external immutable-event observer boundary.
package observer

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

const APIVersion = "1.0"

type MessageType string

const (
	MessageHello    MessageType = "hello"
	MessageEvent    MessageType = "event"
	MessageAck      MessageType = "ack"
	MessageFlush    MessageType = "flush"
	MessageHealth   MessageType = "health"
	MessageShutdown MessageType = "shutdown"
)

type PluginIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Capabilities struct {
	ImmutableEvents  bool `json:"immutable_events"`
	IdempotentEvents bool `json:"idempotent_events"`
	TraceLinkage     bool `json:"trace_linkage,omitempty"`
}

type HelloRequest struct {
	Type                MessageType `json:"type"`
	RequestID           string      `json:"request_id"`
	PluginAPIVersion    string      `json:"plugin_api_version"`
	AgentDVersion       string      `json:"agentd_version"`
	EventSchemaVersions []string    `json:"event_schema_versions"`
}

type HelloResponse struct {
	Type                MessageType    `json:"type"`
	RequestID           string         `json:"request_id"`
	Plugin              PluginIdentity `json:"plugin"`
	PluginAPIVersion    string         `json:"plugin_api_version"`
	EventSchemaVersions []string       `json:"event_schema_versions"`
	Capabilities        Capabilities   `json:"capabilities"`
}

// WireEvent is the immutable durable envelope delivered to observers. Raw is
// encoded as base64 by encoding/json so arbitrary stderr bytes remain exact.
type WireEvent struct {
	SchemaVersion string          `json:"schema_version"`
	EventID       string          `json:"event_id"`
	SessionID     string          `json:"session_id"`
	Generation    int64           `json:"generation"`
	Sequence      int64           `json:"sequence"`
	Timestamp     time.Time       `json:"timestamp"`
	Type          string          `json:"event_type"`
	Stream        durable.Stream  `json:"stream"`
	Payload       json.RawMessage `json:"payload"`
	Raw           []byte          `json:"raw_base64"`
	RawSHA256     string          `json:"raw_sha256"`
}

type EventFrame struct {
	Type       MessageType  `json:"type"`
	DeliveryID string       `json:"delivery_id"`
	Event      WireEvent    `json:"event"`
	Context    EventContext `json:"context"`
}

// EventContext links an immutable event to caller, provider, and sandbox
// identities. RequestManifest is the already-scrubbed durable manifest.
type EventContext struct {
	JobID             string          `json:"job_id"`
	Agent             string          `json:"agent"`
	Runtime           string          `json:"runtime"`
	RequestManifest   json.RawMessage `json:"request_manifest"`
	SecretGrants      []string        `json:"secret_grants,omitempty"`
	ProviderSessionID string          `json:"provider_session_id,omitempty"`
	ImageReference    string          `json:"image_reference,omitempty"`
	ImageDigest       string          `json:"image_digest,omitempty"`
	SandboxProfile    string          `json:"sandbox_profile,omitempty"`
}

type AckStatus string

const (
	AckAccepted  AckStatus = "accepted"
	AckDuplicate AckStatus = "duplicate"
	AckRejected  AckStatus = "rejected"
)

type AckFrame struct {
	Type       MessageType `json:"type"`
	DeliveryID string      `json:"delivery_id"`
	Status     AckStatus   `json:"status"`
	EventID    string      `json:"event_id"`
	SessionID  string      `json:"session_id"`
	Sequence   int64       `json:"sequence"`
	TraceID    string      `json:"trace_id,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type ControlFrame struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
}

type ControlResponse struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Status    string      `json:"status"`
	Error     string      `json:"error,omitempty"`
}

func NewEventFrame(deliveryID string, event durable.Event) EventFrame {
	return NewContextEventFrame(deliveryID, event, EventContext{})
}

func NewContextEventFrame(deliveryID string, event durable.Event, context EventContext) EventFrame {
	return EventFrame{Type: MessageEvent, DeliveryID: deliveryID, Context: context, Event: WireEvent{
		SchemaVersion: event.SchemaVersion, EventID: event.EventID, SessionID: event.SessionID,
		Generation: event.Generation, Sequence: event.Sequence, Timestamp: event.Timestamp,
		Type: event.Type, Stream: event.Stream, Payload: event.Payload, Raw: event.Raw, RawSHA256: event.RawSHA256,
	}}
}

func SupportedMessageTypes() []MessageType {
	return []MessageType{MessageHello, MessageEvent, MessageAck, MessageFlush, MessageHealth, MessageShutdown}
}

func ValidateHandshake(response HelloResponse, requestID, eventSchemaVersion string) error {
	if response.Type != MessageHello || response.RequestID != requestID {
		return fmt.Errorf("observer: hello response identity mismatch")
	}
	if response.Plugin.Name == "" || response.Plugin.Version == "" {
		return fmt.Errorf("observer: plugin identity is required")
	}
	if response.PluginAPIVersion != APIVersion {
		return fmt.Errorf("observer: incompatible plugin API %q", response.PluginAPIVersion)
	}
	if !slices.Contains(response.EventSchemaVersions, eventSchemaVersion) {
		return fmt.Errorf("observer: event schema %q is unsupported", eventSchemaVersion)
	}
	if !response.Capabilities.ImmutableEvents || !response.Capabilities.IdempotentEvents || !response.Capabilities.TraceLinkage {
		return fmt.Errorf("observer: immutable, idempotent, trace-linked event support is required")
	}
	return nil
}

func ValidateAck(ack AckFrame, deliveryID string, event WireEvent) error {
	if ack.Type != MessageAck || ack.DeliveryID != deliveryID {
		return fmt.Errorf("observer: acknowledgement delivery identity mismatch")
	}
	if ack.EventID != event.EventID || ack.SessionID != event.SessionID || ack.Sequence != event.Sequence {
		return fmt.Errorf("observer: acknowledgement event identity mismatch")
	}
	if ack.Status != AckAccepted && ack.Status != AckDuplicate {
		return fmt.Errorf("observer: event rejected by adapter")
	}
	return nil
}
