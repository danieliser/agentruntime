package observer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func TestProcessHandshakeDeliveryAndCleanEnvironment(t *testing.T) {
	t.Setenv("AMBIENT_SECRET_SHOULD_NOT_LEAK", "forbidden")
	process := startHelperProcess(t, "healthy", 2*time.Second)
	defer process.Close(context.Background())
	if process.Identity().Name != "opentraces" {
		t.Fatalf("identity = %+v", process.Identity())
	}
	event := durable.Event{
		SchemaVersion: "1.0", EventID: "event-1", SessionID: "session-1", Generation: 1, Sequence: 1,
		Timestamp: time.Now().UTC(), Type: "content.delta", Stream: durable.StreamProviderStdout,
		Payload: json.RawMessage(`{"text":"hello"}`), Raw: []byte(`{"delta":"hello"}`), RawSHA256: "hash",
	}
	ack, err := process.Deliver(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if ack.EventID != event.EventID || ack.TraceID != "851ad0da-3f90-4ea8-9094-9b644d1913f7" {
		t.Fatalf("ack = %+v", ack)
	}
}

func TestProcessRejectsMismatchedAck(t *testing.T) {
	process := startHelperProcess(t, "bad-ack", 2*time.Second)
	defer process.Close(context.Background())
	_, err := process.Deliver(context.Background(), durable.Event{
		SchemaVersion: "1.0", EventID: "event-1", SessionID: "session-1", Generation: 1,
		Sequence: 1, Timestamp: time.Now().UTC(), Type: "content.delta",
	})
	if err == nil {
		t.Fatal("expected mismatched acknowledgement to fail")
	}
}

func TestProcessTimeoutTerminatesSlowPlugin(t *testing.T) {
	process := startHelperProcess(t, "slow", 25*time.Millisecond)
	_, err := process.Deliver(context.Background(), durable.Event{
		SchemaVersion: "1.0", EventID: "event-1", SessionID: "session-1", Generation: 1,
		Sequence: 1, Timestamp: time.Now().UTC(), Type: "content.delta",
	})
	if err == nil {
		t.Fatal("expected slow observer timeout")
	}
	if process.Running() {
		t.Fatal("timed-out observer process is still running")
	}
}

func TestProcessHealthRejectsDegradedResponse(t *testing.T) {
	process := startHelperProcess(t, "health-error", 2*time.Second)
	defer process.Close(context.Background())
	if err := process.Health(context.Background()); err == nil {
		t.Fatal("expected observer health failure")
	}
}

func startHelperProcess(t *testing.T, mode string, timeout time.Duration) *Process {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := StartProcess(context.Background(), PluginConfig{
		Name: "opentraces", Enabled: true, Command: executable,
		Args:        []string{"-test.run=TestObserverHelperProcess"},
		Environment: map[string]string{"AGENTD_OBSERVER_HELPER": mode}, Timeout: timeout,
	}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func TestObserverHelperProcess(t *testing.T) {
	mode := os.Getenv("AGENTD_OBSERVER_HELPER")
	if mode == "" {
		return
	}
	if os.Getenv("AMBIENT_SECRET_SHOULD_NOT_LEAK") != "" {
		os.Exit(91)
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	encoder := json.NewEncoder(os.Stdout)
	var hello HelloRequest
	if err := decoder.Decode(&hello); err != nil {
		os.Exit(92)
	}
	response := HelloResponse{
		Type: MessageHello, RequestID: hello.RequestID,
		Plugin:           PluginIdentity{Name: "opentraces", Version: "test"},
		PluginAPIVersion: APIVersion, EventSchemaVersions: []string{"1.0"},
		Capabilities: Capabilities{ImmutableEvents: true, IdempotentEvents: true, TraceLinkage: true},
	}
	if mode == "incompatible" {
		response.PluginAPIVersion = "2.0"
	}
	if version := os.Getenv("AGENTD_OBSERVER_VERSION"); version != "" {
		response.Plugin.Version = version
	}
	if err := encoder.Encode(response); err != nil {
		os.Exit(93)
	}
	for {
		var header struct {
			Type MessageType `json:"type"`
		}
		var line json.RawMessage
		if err := decoder.Decode(&line); err != nil {
			return
		}
		if err := json.Unmarshal(line, &header); err != nil {
			os.Exit(94)
		}
		switch header.Type {
		case MessageEvent:
			var frame EventFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				os.Exit(95)
			}
			if mode == "slow" {
				time.Sleep(time.Second)
			}
			if mode == "require-context" && (frame.Context.JobID != "job-observer" || frame.Context.Agent != "codex" || frame.Context.Runtime != "docker" || frame.Context.ImageDigest != "sha256:fixture" || frame.Context.SandboxProfile != "fixture-v1") {
				os.Exit(101)
			}
			ack := AckFrame{Type: MessageAck, DeliveryID: frame.DeliveryID, Status: AckAccepted,
				EventID: frame.Event.EventID, SessionID: frame.Event.SessionID, Sequence: frame.Event.Sequence,
				TraceID: "851ad0da-3f90-4ea8-9094-9b644d1913f7"}
			if mode == "bad-ack" {
				ack.Sequence++
			}
			if recordPath := os.Getenv("AGENTD_OBSERVER_RECORD"); recordPath != "" {
				seen := map[string]bool{}
				if data, err := os.ReadFile(recordPath); err == nil {
					for _, eventID := range strings.Fields(string(data)) {
						seen[eventID] = true
					}
				}
				if seen[frame.Event.EventID] {
					ack.Status = AckDuplicate
				} else {
					file, err := os.OpenFile(recordPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
					if err != nil {
						os.Exit(100)
					}
					_, _ = fmt.Fprintln(file, frame.Event.EventID)
					_ = file.Close()
					if mode == "crash-after-effect" {
						os.Exit(102)
					}
				}
			}
			if err := encoder.Encode(ack); err != nil {
				os.Exit(96)
			}
		case MessageShutdown, MessageFlush, MessageHealth:
			var control ControlFrame
			if err := json.Unmarshal(line, &control); err != nil {
				os.Exit(97)
			}
			status := "ok"
			responseError := ""
			if mode == "health-error" && header.Type == MessageHealth {
				status = "degraded"
				responseError = "external trace system unavailable"
			}
			if err := encoder.Encode(ControlResponse{Type: header.Type, RequestID: control.RequestID, Status: status, Error: responseError}); err != nil {
				os.Exit(98)
			}
			if header.Type == MessageShutdown {
				return
			}
		default:
			fmt.Fprintln(os.Stderr, "unsupported")
			os.Exit(99)
		}
	}
}
