package observer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// TestOpenTracesAdapterQualification launches the separately maintained real
// adapter through AgentD's production clean-environment process boundary. It
// remains opt-in for ordinary AgentD CI because OpenTraces is not an AgentD
// dependency; the cross-repository qualification lane supplies the executable.
func TestOpenTracesAdapterQualification(t *testing.T) {
	command := os.Getenv("AGENTD_TEST_OPENTRACES_ADAPTER")
	if command == "" {
		t.Skip("AGENTD_TEST_OPENTRACES_ADAPTER is not set")
	}
	command, err := filepath.Abs(command)
	if err != nil {
		t.Fatalf("resolve adapter path: %v", err)
	}
	root := filepath.Join(t.TempDir(), "capture")
	opentracesRoot := filepath.Join(t.TempDir(), "opentraces")
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("create qualification project: %v", err)
	}
	marker := []byte(`{"marker_version":"2","project_id":"agentd-qualification","review_policy":"review","push_policy":"manual","remotes":{},"agents":["agentd"]}`)
	if err := os.WriteFile(filepath.Join(project, ".opentraces.json"), marker, 0o600); err != nil {
		t.Fatalf("write qualification marker: %v", err)
	}
	config := PluginConfig{
		Name: "opentraces", Enabled: true, Command: command,
		Environment: map[string]string{
			"OPENTRACES_AGENTD_DIR": root,
			"OT_OPENTRACES_DIR":     opentracesRoot,
		},
		Timeout: 5 * time.Second,
	}
	ctx := context.Background()
	process, err := StartProcess(ctx, config, "qualification", "1.0")
	if err != nil {
		t.Fatalf("start OpenTraces adapter: %v", err)
	}
	if err := process.Health(ctx); err != nil {
		t.Fatalf("OpenTraces health: %v", err)
	}

	manifest, err := json.Marshal(map[string]any{
		"agent": "codex", "runtime": "docker", "prompt": "qualify local capture",
		"work_dir": project, "model": "gpt-5.6-sol",
	})
	if err != nil {
		t.Fatalf("encode qualification manifest: %v", err)
	}
	eventContext := EventContext{
		JobID: "trading-floor-job-qualification", Agent: "codex", Runtime: "docker",
		RequestManifest: manifest, ProviderSessionID: "codex-qualification",
		ImageDigest: "sha256:qualification", SandboxProfile: "docker-native-v1",
	}
	events := []durable.Event{
		qualificationEvent(t, 1, "content.delta", durable.StreamProviderStdout, json.RawMessage(`{"text":"hello"}`)),
		qualificationEvent(t, 2, "tool.call", durable.StreamProviderStdout, json.RawMessage(`{"calls":[{"id":"call-1","name":"Read","input":{"path":"README.md"}}]}`)),
		qualificationEvent(t, 3, "session.completed", durable.StreamTerminal, json.RawMessage(`{"reason":"completed","exit_code":0}`)),
	}
	var first AckFrame
	for index, event := range events {
		ack, err := process.DeliverWithContext(ctx, event, eventContext)
		if err != nil {
			t.Fatalf("deliver event %d: %v", event.Sequence, err)
		}
		if ack.Status != AckAccepted || ack.TraceID == "" {
			t.Fatalf("event %d ack = %+v", event.Sequence, ack)
		}
		if index == 0 {
			first = ack
		} else if ack.TraceID != first.TraceID {
			t.Fatalf("trace link changed from %s to %s", first.TraceID, ack.TraceID)
		}
	}
	if err := process.Health(ctx); err != nil {
		t.Fatalf("health after terminal capture: %v", err)
	}
	queryable, err := filepath.Glob(filepath.Join(
		opentracesRoot, "projects", "*", "traces", first.TraceID+".jsonl",
	))
	if err != nil || len(queryable) != 1 {
		t.Fatalf("queryable trace paths = %v, err=%v", queryable, err)
	}
	if err := process.Close(ctx); err != nil {
		t.Fatalf("close first adapter: %v", err)
	}

	restarted, err := StartProcess(ctx, config, "qualification", "1.0")
	if err != nil {
		t.Fatalf("restart OpenTraces adapter: %v", err)
	}
	defer restarted.Close(ctx)
	duplicate, err := restarted.DeliverWithContext(ctx, events[len(events)-1], eventContext)
	if err != nil {
		t.Fatalf("redeliver after restart: %v", err)
	}
	if duplicate.Status != AckDuplicate || duplicate.TraceID != first.TraceID {
		t.Fatalf("restart ack = %+v, first = %+v", duplicate, first)
	}
}

func qualificationEvent(
	t *testing.T,
	sequence int64,
	eventType string,
	stream durable.Stream,
	payload json.RawMessage,
) durable.Event {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"qualification": true, "sequence": sequence, "type": eventType,
	})
	if err != nil {
		t.Fatalf("encode event raw: %v", err)
	}
	digest := sha256.Sum256(raw)
	return durable.Event{
		SchemaVersion: "1.0", EventID: fmt.Sprintf("evt_opentraces_qualification_%d", sequence),
		SessionID: "11111111-1111-4111-8111-111111111111", Generation: 1, Sequence: sequence,
		Timestamp: time.Date(2026, 8, 9, 12, 0, int(sequence), 0, time.UTC), Type: eventType,
		Stream: stream, Payload: payload, Raw: raw, RawSHA256: hex.EncodeToString(digest[:]),
	}
}
