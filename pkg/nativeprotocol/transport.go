package nativeprotocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"
)

const (
	transportBufferSize = 64
	maxNativeRecordSize = 16 << 20
)

// StreamTransport frames exact stdout/stderr lines and writes typed native
// inputs. It does not normalize or merge the two output streams.
type StreamTransport struct {
	adapter  Adapter
	process  ProcessIO
	recovery RecoveryMetadata

	mu          sync.RWMutex
	started     bool
	closed      bool
	providerID  string
	turnID      string
	nextRPCID   int64
	readErr     error
	inputPolicy InputPolicy
	pending     map[string]chan codexRPCResponse

	writeMu   sync.Mutex
	closeOnce sync.Once
	readers   sync.WaitGroup
	records   chan Record
	stderr    chan Record
	wait      chan Exit
	done      chan struct{}
}

type codexRPCResponse struct {
	result json.RawMessage
	err    error
}

// NewStreamTransport builds a transport around an already-created native
// process/container connection. Start must be called before use.
func NewStreamTransport(adapter Adapter, process ProcessIO, recovery RecoveryMetadata) *StreamTransport {
	return &StreamTransport{
		adapter: adapter, process: process, recovery: recovery, nextRPCID: 1,
		pending: make(map[string]chan codexRPCResponse),
		records: make(chan Record, transportBufferSize),
		stderr:  make(chan Record, transportBufferSize),
		wait:    make(chan Exit, 1),
		done:    make(chan struct{}),
	}
}

func (transport *StreamTransport) Start(_ context.Context) error {
	const op = "start_native_transport"
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return newError(CodeInvalidState, op, "transport is closed", nil)
	}
	if transport.started {
		return nil
	}
	if transport.adapter == nil || transport.process.Stdout == nil || transport.process.Wait == nil {
		return newError(CodeInvalidArgument, op, "adapter, stdout, and wait channel are required", nil)
	}
	transport.started = true
	transport.readers.Add(1)
	go transport.readStream(transport.process.Stdout, StreamProviderStdout, transport.records)
	if transport.process.Stderr != nil {
		transport.readers.Add(1)
		go transport.readStream(transport.process.Stderr, StreamRuntimeStderr, transport.stderr)
	}
	go transport.waitForExit()
	return nil
}

func (transport *StreamTransport) Send(ctx context.Context, input Input) error {
	const op = "send_native_input"
	transport.mu.Lock()
	if !transport.started || transport.closed {
		transport.mu.Unlock()
		return newError(CodeInvalidState, op, "transport is not running", nil)
	}
	if input.ProviderID == "" {
		input.ProviderID = transport.providerID
	}
	if input.TurnID == "" {
		input.TurnID = transport.turnID
	}
	if len(input.RequestID) == 0 && transport.adapter.Provider() == ProviderCodex {
		input.RequestID = json.RawMessage(strconv.FormatInt(transport.nextRPCID, 10))
		transport.nextRPCID++
	}
	if input.ProviderID != "" {
		transport.providerID = input.ProviderID
	}
	if transport.inputPolicy.Enforced {
		input.Policy = transport.inputPolicy
	}
	transport.mu.Unlock()

	messages, err := transport.adapter.Encode(input)
	if err != nil {
		return err
	}
	return transport.writeMessages(ctx, op, messages)
}

// Bootstrap initializes Codex app-server and opens or resumes its thread.
// Claude's CLI needs no handshake; its optional provider ID is retained for
// subsequent native input.
func (transport *StreamTransport) Bootstrap(ctx context.Context, request BootstrapRequest) error {
	const op = "bootstrap_native_transport"
	transport.mu.Lock()
	if !transport.started || transport.closed {
		transport.mu.Unlock()
		return newError(CodeInvalidState, op, "transport is not running", nil)
	}
	transport.inputPolicy = request.Policy
	if request.Reconnect {
		if request.ProviderID != "" {
			transport.providerID = request.ProviderID
		}
		transport.mu.Unlock()
		return nil
	}
	if transport.adapter.Provider() == ProviderClaude {
		transport.providerID = request.ProviderID
		transport.mu.Unlock()
		return nil
	}
	transport.mu.Unlock()
	clientName := request.ClientName
	if clientName == "" {
		clientName = "agentruntime"
	}
	clientVersion := request.ClientVersion
	if clientVersion == "" {
		clientVersion = "dev"
	}
	result, err := transport.callCodex(ctx, json.RawMessage("0"), "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": clientName, "version": clientVersion},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err != nil {
		return err
	}
	var initializeResult struct {
		UserAgent string `json:"userAgent"`
	}
	if err := json.Unmarshal(result, &initializeResult); err != nil || initializeResult.UserAgent == "" {
		return newError(CodeDecode, op, "Codex initialize response is missing userAgent", err)
	}
	initialized, err := json.Marshal(map[string]any{"method": "initialized", "params": map[string]any{}})
	if err != nil {
		return newError(CodeEncode, op, "encode initialized notification", err)
	}
	if err := transport.writeMessages(ctx, op, [][]byte{initialized}); err != nil {
		return err
	}
	id := transport.allocateRPCID()
	method := "thread/start"
	params := map[string]any{}
	if request.Policy.Enforced {
		sandbox := "read-only"
		if request.Policy.Filesystem == "workspace_write" {
			sandbox = "workspace-write"
		}
		params["approvalPolicy"] = request.Policy.ApprovalPolicy
		params["sandbox"] = sandbox
		params["cwd"] = "/workspace"
		params["dynamicTools"] = []any{}
		params["environments"] = []any{}
	}
	if request.ProviderID != "" {
		method = "thread/resume"
		params["threadId"] = request.ProviderID
	}
	threadResult, err := transport.callCodex(ctx, json.RawMessage(strconv.FormatInt(id, 10)), method, params)
	if err != nil {
		return err
	}
	threadID := request.ProviderID
	if threadID == "" {
		threadID = codexThreadID(threadResult)
	}
	if threadID == "" {
		return newError(CodeDecode, op, "Codex thread response is missing thread ID", nil)
	}
	transport.mu.Lock()
	transport.providerID = threadID
	transport.mu.Unlock()
	return nil
}

func (transport *StreamTransport) callCodex(ctx context.Context, id json.RawMessage, method string, params any) (json.RawMessage, error) {
	const op = "call_codex_rpc"
	key := string(id)
	response := make(chan codexRPCResponse, 1)
	transport.mu.Lock()
	if transport.closed {
		transport.mu.Unlock()
		return nil, newError(CodeInvalidState, op, "transport is closed", nil)
	}
	transport.pending[key] = response
	transport.mu.Unlock()
	message, err := json.Marshal(map[string]any{"id": json.RawMessage(id), "method": method, "params": params})
	if err != nil {
		transport.removePending(key)
		return nil, newError(CodeEncode, op, "encode Codex RPC", err)
	}
	if err := transport.writeMessages(ctx, op, [][]byte{message}); err != nil {
		transport.removePending(key)
		return nil, err
	}
	select {
	case <-ctx.Done():
		transport.removePending(key)
		return nil, ctx.Err()
	case <-transport.done:
		transport.removePending(key)
		return nil, newError(CodeTransport, op, "transport exited before Codex response", nil)
	case result := <-response:
		return result.result, result.err
	}
}

func (transport *StreamTransport) allocateRPCID() int64 {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	id := transport.nextRPCID
	transport.nextRPCID++
	return id
}

func (transport *StreamTransport) removePending(key string) {
	transport.mu.Lock()
	delete(transport.pending, key)
	transport.mu.Unlock()
}

func (transport *StreamTransport) writeMessages(ctx context.Context, op string, messages [][]byte) error {
	transport.writeMu.Lock()
	defer transport.writeMu.Unlock()
	for _, message := range messages {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if transport.process.Stdin == nil {
			return newError(CodeInvalidState, op, "native stdin is closed", nil)
		}
		if _, err := transport.process.Stdin.Write(append(append([]byte(nil), message...), '\n')); err != nil {
			return newError(CodeTransport, op, "write native input", err)
		}
	}
	return nil
}

func (transport *StreamTransport) Interrupt(ctx context.Context) error {
	return transport.Send(ctx, Input{Kind: InputInterrupt})
}

func (transport *StreamTransport) Records() <-chan Record { return transport.records }

func (transport *StreamTransport) Stderr() <-chan Record { return transport.stderr }

func (transport *StreamTransport) Wait() <-chan Exit { return transport.wait }

func (transport *StreamTransport) RecoveryMetadata() RecoveryMetadata { return transport.recovery }

func (transport *StreamTransport) Close() error {
	var closeErr error
	transport.closeOnce.Do(func() {
		transport.mu.Lock()
		transport.closed = true
		transport.mu.Unlock()
		if transport.process.Stdin != nil {
			if err := transport.process.Stdin.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				closeErr = err
			}
		}
		if transport.process.Kill != nil {
			if err := transport.process.Kill(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

func (transport *StreamTransport) readStream(reader io.ReadCloser, stream Stream, target chan<- Record) {
	defer transport.readers.Done()
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxNativeRecordSize)
	var ordinal int64
	for scanner.Scan() {
		ordinal++
		raw := append([]byte(nil), scanner.Bytes()...)
		if stream == StreamProviderStdout {
			if transport.adapter.Provider() == ProviderCodex {
				transport.deliverCodexResponse(raw)
			}
			derived, err := transport.adapter.Decode(raw)
			if err == nil {
				transport.mu.Lock()
				if derived.ProviderID != "" {
					transport.providerID = derived.ProviderID
				}
				if derived.TurnID != "" {
					transport.turnID = derived.TurnID
				}
				transport.mu.Unlock()
			}
		}
		target <- Record{
			Provider: transport.adapter.Provider(), Stream: stream, Ordinal: ordinal,
			Timestamp: time.Now().UTC().Round(0), Raw: raw,
		}
	}
	if err := scanner.Err(); err != nil {
		transport.mu.Lock()
		transport.readErr = errors.Join(transport.readErr, newError(CodeTransport, "read_native_stream", "read native stream", err))
		transport.mu.Unlock()
	}
}

func (transport *StreamTransport) waitForExit() {
	exit, ok := <-transport.process.Wait
	if !ok {
		exit = Exit{Err: io.EOF}
	}
	transport.readers.Wait()
	transport.mu.Lock()
	if transport.readErr != nil {
		exit.Err = errors.Join(exit.Err, transport.readErr)
	}
	transport.closed = true
	transport.mu.Unlock()
	close(transport.records)
	close(transport.stderr)
	transport.wait <- exit
	close(transport.wait)
	close(transport.done)
}

func (transport *StreamTransport) deliverCodexResponse(raw []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Method != "" || len(envelope.ID) == 0 {
		return
	}
	key := string(envelope.ID)
	transport.mu.Lock()
	pending := transport.pending[key]
	if pending != nil {
		delete(transport.pending, key)
	}
	transport.mu.Unlock()
	if pending == nil {
		return
	}
	response := codexRPCResponse{result: append(json.RawMessage(nil), envelope.Result...)}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		response.err = newError(CodeTransport, "call_codex_rpc", "Codex RPC returned an error", nil)
	}
	pending <- response
	close(pending)
}

func codexThreadID(raw json.RawMessage) string {
	var result struct {
		ThreadID string `json:"threadId"`
		ID       string `json:"id"`
		Thread   struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return ""
	}
	for _, value := range []string{result.ThreadID, result.ID, result.Thread.ThreadID, result.Thread.ID} {
		if value != "" {
			return value
		}
	}
	return ""
}

var _ Transport = (*StreamTransport)(nil)
