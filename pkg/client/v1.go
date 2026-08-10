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
	"strings"
	"time"

	"github.com/danieliser/agentruntime/pkg/api"
)

// DurableSession is the public v1 logical-session view.
type DurableSession struct {
	SessionID           string                `json:"session_id"`
	IdempotencyKey      string                `json:"idempotency_key"`
	Agent               string                `json:"agent"`
	Runtime             string                `json:"runtime"`
	State               string                `json:"state"`
	Generation          int64                 `json:"generation"`
	LastSequence        int64                 `json:"last_sequence"`
	CreatedAt           time.Time             `json:"created_at"`
	UpdatedAt           time.Time             `json:"updated_at"`
	EventsURL           string                `json:"events_url"`
	EventStreamURL      string                `json:"event_stream_url"`
	ExecutionPolicy     *api.ExecutionPolicy  `json:"execution_policy,omitempty"`
	ExecutionPolicyHash string                `json:"execution_policy_hash,omitempty"`
	StructuredOutput    *api.StructuredOutput `json:"structured_output,omitempty"`
	OutputSchemaHash    string                `json:"output_schema_hash,omitempty"`
}

// StructuredResult is the exact validated final JSON artifact and its durable
// event identity.
type StructuredResult struct {
	Bytes    []byte
	SHA256   string
	EventID  string
	Sequence int64
}

type ReplayCapabilities struct {
	SequenceCursor     bool `json:"sequence_cursor"`
	StoredThenLive     bool `json:"stored_then_live"`
	RestartPersistence bool `json:"restart_persistence"`
}

type AuthenticationCapabilities struct {
	Mode               string `json:"mode"`
	Transport          string `json:"transport"`
	HTTPTransport      string `json:"http_transport"`
	WebSocketTransport string `json:"websocket_transport"`
}

type StructuredOutputCapabilities struct {
	Providers       []string `json:"providers"`
	NativeEnforced  bool     `json:"native_enforced"`
	SchemaHash      string   `json:"schema_hash"`
	DefaultMaxBytes int      `json:"default_max_bytes"`
	MaximumBytes    int      `json:"maximum_bytes"`
	ResultEvent     string   `json:"result_event"`
	ResultEndpoint  string   `json:"result_endpoint"`
}

type WorkspaceProfileCapabilities struct {
	Name               string   `json:"name"`
	Retention          string   `json:"retention"`
	Filesystems        []string `json:"filesystems"`
	Network            string   `json:"network"`
	HostMounts         bool     `json:"host_mounts"`
	AmbientCredentials bool     `json:"ambient_credentials"`
}

type CredentialGrantCapabilities struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	RequestEnv      string `json:"request_env"`
	Materialization string `json:"materialization"`
	Persistence     string `json:"persistence"`
}

// Capabilities is the v1 compatibility handshake a caller checks before
// submitting paid work.
type Capabilities struct {
	AgentDVersion           string                         `json:"agentd_version"`
	CommitHash              string                         `json:"commit_hash"`
	APIVersions             []string                       `json:"api_versions"`
	EventSchemaVersions     []string                       `json:"event_schema_versions"`
	ExecutionPolicyVersions []string                       `json:"execution_policy_versions"`
	NativeProviders         []string                       `json:"native_providers"`
	Runtimes                []string                       `json:"runtimes"`
	LifecycleControls       []string                       `json:"lifecycle_controls"`
	Replay                  ReplayCapabilities             `json:"replay"`
	DockerReconstruction    bool                           `json:"docker_reconstruction"`
	PluginAPIVersions       []string                       `json:"plugin_api_versions"`
	Plugins                 []PluginStatus                 `json:"plugins"`
	ListenerScope           string                         `json:"listener_scope"`
	Authentication          AuthenticationCapabilities     `json:"authentication"`
	StructuredOutput        StructuredOutputCapabilities   `json:"structured_output"`
	WorkspaceProfiles       []WorkspaceProfileCapabilities `json:"workspace_profiles"`
	CredentialGrants        []CredentialGrantCapabilities  `json:"credential_grants"`
}

type PluginStatus struct {
	Name           string    `json:"name"`
	Version        string    `json:"version,omitempty"`
	Policy         string    `json:"policy"`
	State          string    `json:"state"`
	LastError      string    `json:"last_error,omitempty"`
	Unacknowledged int64     `json:"unacknowledged_events"`
	CheckedAt      time.Time `json:"checked_at,omitempty"`
}

type TraceLink struct {
	Plugin               string `json:"plugin"`
	SessionID            string `json:"session_id"`
	TraceID              string `json:"trace_id"`
	AcknowledgedSequence int64  `json:"acknowledged_sequence"`
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

// TerminalReceipt is the immutable terminal proof for one logical session.
type TerminalReceipt struct {
	SessionID    string    `json:"session_id"`
	Generation   int64     `json:"generation"`
	State        string    `json:"state"`
	Reason       string    `json:"reason"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	Signal       string    `json:"signal,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	OutputHash   string    `json:"output_hash"`
	ArtifactHash string    `json:"artifact_hash"`
	LastSequence int64     `json:"last_sequence"`
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

// ListDurableSessions returns active and terminal logical sessions from the
// durable store. It is the canonical history/listing surface for v1 clients.
func (c *Client) ListDurableSessions(ctx context.Context) ([]DurableSession, error) {
	httpRequest, err := c.newRequest(ctx, http.MethodGet, "/api/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data []DurableSession `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data == nil {
		envelope.Data = []DurableSession{}
	}
	return envelope.Data, nil
}

func (c *Client) ListPlugins(ctx context.Context) ([]PluginStatus, error) {
	httpRequest, err := c.newRequest(ctx, http.MethodGet, "/api/v1/plugins", nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data []PluginStatus `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data == nil {
		envelope.Data = []PluginStatus{}
	}
	return envelope.Data, nil
}

func (c *Client) GetTraceLinks(ctx context.Context, sessionID string) ([]TraceLink, error) {
	httpRequest, err := c.newRequest(ctx, http.MethodGet, v1SessionPath(sessionID)+"/traces", nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data []TraceLink `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data == nil {
		envelope.Data = []TraceLink{}
	}
	return envelope.Data, nil
}

// GetTerminalReceipt returns the immutable terminal proof for a session.
func (c *Client) GetTerminalReceipt(ctx context.Context, sessionID string) (*TerminalReceipt, error) {
	httpRequest, err := c.newRequest(ctx, http.MethodGet, v1SessionPath(sessionID)+"/receipt", nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data TerminalReceipt `json:"data"`
	}
	if err := c.doJSON(httpRequest, &envelope); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// SendInput submits a durable prompt or steer operation to an active native
// provider generation.
func (c *Client) SendInput(ctx context.Context, sessionID, idempotencyKey, kind, content string) error {
	return c.postV1Control(ctx, v1SessionPath(sessionID)+"/input", map[string]any{
		"idempotency_key": idempotencyKey,
		"kind":            kind,
		"text":            content,
	})
}

// Interrupt requests provider-native interruption without terminating the
// logical session.
func (c *Client) Interrupt(ctx context.Context, sessionID, idempotencyKey string) error {
	return c.postV1Control(ctx, v1SessionPath(sessionID)+"/interrupt", map[string]string{"idempotency_key": idempotencyKey})
}

// Cancel records caller cancellation and stops the active native generation.
func (c *Client) Cancel(ctx context.Context, sessionID, idempotencyKey string) error {
	return c.postV1Control(ctx, v1SessionPath(sessionID)+"/cancel", map[string]string{"idempotency_key": idempotencyKey})
}

// Terminate administratively stops the active generation and retains a
// distinct terminated receipt reason.
func (c *Client) Terminate(ctx context.Context, sessionID, idempotencyKey string) error {
	return c.postV1Control(ctx, v1SessionPath(sessionID)+"/terminate", map[string]string{"idempotency_key": idempotencyKey})
}

// Resume starts generation N+1 for a recoverable lost native generation.
func (c *Client) Resume(ctx context.Context, sessionID, prompt string, env map[string]string) (*DurableSession, error) {
	httpRequest, err := c.newJSONRequest(ctx, http.MethodPost, v1SessionPath(sessionID)+"/resume", map[string]any{
		"prompt": prompt,
		"env":    env,
	})
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

func (c *Client) postV1Control(ctx context.Context, path string, body any) error {
	httpRequest, err := c.newJSONRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	return c.doJSON(httpRequest, nil)
}

func v1SessionPath(sessionID string) string {
	return "/api/v1/sessions/" + url.PathEscape(sessionID)
}

// GetStructuredResult returns the exact bytes committed in output.final and
// verifies the server-provided content hash before returning them.
func (c *Client) GetStructuredResult(ctx context.Context, sessionID string) (*StructuredResult, error) {
	request, err := c.newRequest(ctx, http.MethodGet, v1SessionPath(sessionID)+"/result", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, api.MaximumStructuredOutputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read structured result: %w", err)
	}
	if len(raw) > api.MaximumStructuredOutputBytes {
		return nil, fmt.Errorf("structured result exceeds maximum size")
	}
	wantHash := response.Header.Get("X-Content-SHA256")
	digest := sha256.Sum256(raw)
	actualHash := "sha256:" + hex.EncodeToString(digest[:])
	if wantHash == "" || !strings.EqualFold(wantHash, actualHash) {
		return nil, fmt.Errorf("structured result hash mismatch")
	}
	sequence, err := strconv.ParseInt(response.Header.Get("X-Event-Sequence"), 10, 64)
	if err != nil || sequence < 1 {
		return nil, fmt.Errorf("structured result has invalid event sequence")
	}
	return &StructuredResult{
		Bytes: append([]byte(nil), raw...), SHA256: actualHash,
		EventID: response.Header.Get("X-Event-ID"), Sequence: sequence,
	}, nil
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
			if event.Type == "output.final" {
				continue
			}
			if event.Stream == "terminal" && strings.HasPrefix(event.Type, "session.") {
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
