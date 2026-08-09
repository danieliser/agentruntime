package observer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/durable"
)

type Process struct {
	config   PluginConfig
	command  *exec.Cmd
	encoder  *json.Encoder
	decoder  *json.Decoder
	stdin    io.WriteCloser
	identity PluginIdentity
	running  atomic.Bool
	done     chan struct{}
	mu       sync.Mutex
}

func StartProcess(ctx context.Context, config PluginConfig, agentdVersion, eventSchemaVersion string) (*Process, error) {
	if !config.Enabled || !safeName.MatchString(config.Name) || config.Command == "" {
		return nil, fmt.Errorf("observer: enabled plugin with explicit command is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	command := exec.Command(config.Command, config.Args...)
	command.Env = explicitEnvironment(config.Environment)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("observer: open plugin stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("observer: open plugin stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("observer: open plugin stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("observer: start plugin %q: %w", config.Name, err)
	}
	process := &Process{
		config: config, command: command, encoder: json.NewEncoder(stdin),
		decoder: json.NewDecoder(bufio.NewReader(stdout)), stdin: stdin, done: make(chan struct{}),
	}
	process.running.Store(true)
	go func() {
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go func() {
		_ = command.Wait()
		process.running.Store(false)
		close(process.done)
	}()
	requestID := uuid.NewString()
	request := HelloRequest{
		Type: MessageHello, RequestID: requestID, PluginAPIVersion: APIVersion,
		AgentDVersion: agentdVersion, EventSchemaVersions: []string{eventSchemaVersion},
	}
	var response HelloResponse
	if err := process.exchange(ctx, request, &response); err != nil {
		process.terminate()
		return nil, fmt.Errorf("observer: plugin %q handshake: %w", config.Name, err)
	}
	if err := ValidateHandshake(response, requestID, eventSchemaVersion); err != nil {
		process.terminate()
		return nil, err
	}
	if response.Plugin.Name != config.Name {
		process.terminate()
		return nil, fmt.Errorf("observer: configured plugin %q identified as %q", config.Name, response.Plugin.Name)
	}
	process.identity = response.Plugin
	return process, nil
}

func explicitEnvironment(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+environment[key])
	}
	return result
}

func (process *Process) Identity() PluginIdentity { return process.identity }

func (process *Process) Running() bool { return process != nil && process.running.Load() }

func (process *Process) Deliver(ctx context.Context, event durable.Event) (AckFrame, error) {
	return process.DeliverWithContext(ctx, event, EventContext{})
}

func (process *Process) DeliverWithContext(ctx context.Context, event durable.Event, eventContext EventContext) (AckFrame, error) {
	if process == nil || !process.Running() {
		return AckFrame{}, fmt.Errorf("observer: plugin is not running")
	}
	frame := NewContextEventFrame(event.EventID, event, eventContext)
	var ack AckFrame
	if err := process.exchange(ctx, frame, &ack); err != nil {
		return AckFrame{}, err
	}
	if err := ValidateAck(ack, frame.DeliveryID, frame.Event); err != nil {
		process.terminate()
		return AckFrame{}, err
	}
	return ack, nil
}

func (process *Process) Flush(ctx context.Context) error {
	if process == nil || !process.Running() {
		return nil
	}
	return process.control(ctx, MessageFlush)
}

func (process *Process) Health(ctx context.Context) error {
	return process.control(ctx, MessageHealth)
}

func (process *Process) control(ctx context.Context, messageType MessageType) error {
	if process == nil || !process.Running() {
		return fmt.Errorf("observer: plugin is not running")
	}
	requestID := uuid.NewString()
	var response ControlResponse
	if err := process.exchange(ctx, ControlFrame{Type: messageType, RequestID: requestID}, &response); err != nil {
		return err
	}
	if response.Type != messageType || response.RequestID != requestID || response.Status != "ok" {
		return fmt.Errorf("observer: %s status %q", messageType, response.Status)
	}
	return nil
}

func (process *Process) exchange(ctx context.Context, request, response any) error {
	process.mu.Lock()
	defer process.mu.Unlock()
	if !process.Running() {
		return fmt.Errorf("observer: plugin is not running")
	}
	if err := process.encoder.Encode(request); err != nil {
		process.terminate()
		return fmt.Errorf("observer: write plugin request: %w", err)
	}
	result := make(chan error, 1)
	go func() { result <- process.decoder.Decode(response) }()
	timer := time.NewTimer(process.config.Timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		if err != nil {
			process.terminate()
			return fmt.Errorf("observer: read plugin response: %w", err)
		}
		return nil
	case <-ctx.Done():
		process.terminate()
		return ctx.Err()
	case <-timer.C:
		process.terminate()
		return fmt.Errorf("observer: plugin response timeout after %s", process.config.Timeout)
	}
}

func (process *Process) Close(ctx context.Context) error {
	if process == nil || !process.Running() {
		return nil
	}
	requestID := uuid.NewString()
	var response ControlResponse
	err := process.exchange(ctx, ControlFrame{Type: MessageShutdown, RequestID: requestID}, &response)
	if err == nil && (response.Type != MessageShutdown || response.RequestID != requestID || response.Status != "ok") {
		err = fmt.Errorf("observer: invalid shutdown response")
	}
	_ = process.stdin.Close()
	select {
	case <-process.done:
	case <-ctx.Done():
		process.terminate()
		<-process.done
	}
	return err
}

func (process *Process) terminate() {
	if process != nil && process.command != nil && process.command.Process != nil && process.running.Swap(false) {
		_ = process.command.Process.Kill()
	}
}
