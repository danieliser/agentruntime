// Package nativeprotocol defines AgentD's provider-native JSON transport and
// derives stable semantic views without replacing the original record bytes.
package nativeprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Provider identifies a native provider protocol.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// Stream keeps provider stdout and runtime stderr as separate authorities.
type Stream string

const (
	StreamProviderStdout Stream = "provider_stdout"
	StreamRuntimeStderr  Stream = "runtime_stderr"
)

// InputKind identifies a caller command on a native provider connection.
type InputKind string

const (
	InputPrompt    InputKind = "prompt"
	InputSteer     InputKind = "steer"
	InputInterrupt InputKind = "interrupt"
	InputApproval  InputKind = "approval"
)

// Input is one typed provider command. ProviderID is Claude's session ID or
// Codex's thread ID; TurnID is required only where the provider requires it.
type Input struct {
	Kind          InputKind
	Text          string
	ProviderID    string
	TurnID        string
	RequestID     json.RawMessage
	ApprovalAllow bool
}

// DerivedEvent is the query-friendly view of one exact native record.
type DerivedEvent struct {
	Type       string
	Payload    json.RawMessage
	ProviderID string
	TurnID     string
}

// Record is one exact line read from a native transport.
type Record struct {
	Provider  Provider
	Stream    Stream
	Ordinal   int64
	Timestamp time.Time
	Raw       []byte
}

// Exit is the process-level terminal result.
type Exit struct {
	Code   int
	Signal string
	Err    error
}

// RecoveryMetadata identifies the runtime incarnation behind a transport.
type RecoveryMetadata struct {
	SessionID  string
	Generation int64
	RuntimeID  string
}

// BootstrapRequest initializes a provider-native connection and optionally
// reopens an existing provider session/thread before input is sent.
type BootstrapRequest struct {
	ProviderID    string
	ClientName    string
	ClientVersion string
	// Reconnect attaches to an app-server process that AgentD initialized
	// previously. It restores local correlation state without replaying the
	// provider handshake on the already-running process.
	Reconnect bool
}

// ProcessIO is the already-created process/container connection used by the
// protocol transport. Runtime creation remains owned by the runtime package.
type ProcessIO struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	Wait   <-chan Exit
	Kill   func() error
}

// Adapter owns only provider wire encoding and derived-event decoding.
type Adapter interface {
	Provider() Provider
	Encode(Input) ([][]byte, error)
	Decode([]byte) (DerivedEvent, error)
}

// Transport is the single NAT-201 runtime-neutral native connection contract.
type Transport interface {
	Start(context.Context) error
	Bootstrap(context.Context, BootstrapRequest) error
	Send(context.Context, Input) error
	Interrupt(context.Context) error
	Records() <-chan Record
	Stderr() <-chan Record
	Wait() <-chan Exit
	RecoveryMetadata() RecoveryMetadata
	Close() error
}

// ErrorCode is a stable native-protocol failure classification.
type ErrorCode string

const (
	CodeInvalidArgument ErrorCode = "invalid_argument"
	CodeInvalidState    ErrorCode = "invalid_state"
	CodeEncode          ErrorCode = "encode_failed"
	CodeDecode          ErrorCode = "decode_failed"
	CodeTransport       ErrorCode = "transport_failed"
)

// Error is returned at native protocol boundaries instead of requiring string
// matching by callers.
type Error struct {
	Code    ErrorCode
	Op      string
	Message string
	Err     error
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.Err != nil {
		return fmt.Sprintf("%s: %s: %s: %v", err.Op, err.Code, err.Message, err.Err)
	}
	return fmt.Sprintf("%s: %s: %s", err.Op, err.Code, err.Message)
}

func (err *Error) Unwrap() error { return err.Err }

func newError(code ErrorCode, op, message string, cause error) error {
	return &Error{Code: code, Op: op, Message: message, Err: cause}
}
