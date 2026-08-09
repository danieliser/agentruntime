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

	mu         sync.RWMutex
	started    bool
	closed     bool
	providerID string
	turnID     string
	nextRPCID  int64
	readErr    error

	writeMu   sync.Mutex
	closeOnce sync.Once
	readers   sync.WaitGroup
	records   chan Record
	stderr    chan Record
	wait      chan Exit
}

// NewStreamTransport builds a transport around an already-created native
// process/container connection. Start must be called before use.
func NewStreamTransport(adapter Adapter, process ProcessIO, recovery RecoveryMetadata) *StreamTransport {
	return &StreamTransport{
		adapter: adapter, process: process, recovery: recovery, nextRPCID: 1,
		records: make(chan Record, transportBufferSize),
		stderr:  make(chan Record, transportBufferSize),
		wait:    make(chan Exit, 1),
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
	transport.mu.Unlock()

	messages, err := transport.adapter.Encode(input)
	if err != nil {
		return err
	}
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
}

var _ Transport = (*StreamTransport)(nil)
