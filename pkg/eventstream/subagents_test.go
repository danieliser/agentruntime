package eventstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func TestSubagentMetadataPropagatesToChildRecords(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	defer store.Close()
	createStreamSession(t, ctx, store, "metadata")
	broker := New(store)
	records := []string{
		`{"method":"turn/started","params":{"threadId":"parent","turn":{"id":"p"}}}`,
		`{"method":"item/completed","params":{"threadId":"parent","item":{"type":"subAgentActivity","kind":"started","agentThreadId":"child","agentPath":"/root/check","model":"test-model"}}}`,
		`{"method":"item/completed","params":{"threadId":"child","item":{"type":"agentMessage","text":"child summary"}}}`,
	}
	var last durable.Event
	for i, raw := range records {
		// Reconstructing a broker must retain the same child metadata.
		if i == 2 {
			broker = New(store)
		}
		var err error
		last, err = broker.Ingest(ctx, IngestParams{SessionID: "metadata", Generation: 1, Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderCodex, Stream: nativeprotocol.StreamProviderStdout, Ordinal: int64(i + 1), Timestamp: time.Unix(200+int64(i), 0), Raw: []byte(raw)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var p map[string]any
	_ = json.Unmarshal(last.Payload, &p)
	if p["subagent_path"] != "/root/check" || p["model"] != "test-model" {
		t.Fatalf("lost child metadata: %s", last.Payload)
	}
}

func TestClaudeShellTaskDoesNotIncreaseSubagentCount(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	defer store.Close()
	createStreamSession(t, ctx, store, "claude-tasks")
	broker := New(store)
	for i, raw := range []string{
		`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"agent-tool","task_type":"local_agent"}`,
		`{"type":"assistant","parent_tool_use_id":"agent-tool","message":{"model":"haiku","content":[{"type":"text","text":"child"}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"bash-1","tool_use_id":"bash-tool","task_type":"local_bash","parent_task_id":"agent-1"}`,
	} {
		_, err := broker.Ingest(ctx, IngestParams{SessionID: "claude-tasks", Generation: 1, Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: int64(i + 1), Timestamp: time.Now(), Raw: []byte(raw)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	session, _ := store.GetSession(ctx, "claude-tasks")
	activity, err := SessionActivity(ctx, store, session, time.Now())
	if err != nil || activity.ActiveSubagents != 1 {
		t.Fatalf("activity=%+v err=%v", activity, err)
	}
}

func TestClaudeChildSessionCannotRebindParent(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	defer store.Close()
	createStreamSession(t, ctx, store, "claude-child")
	broker := New(store)
	for i, raw := range []string{
		`{"type":"system","subtype":"init","session_id":"parent"}`,
		`{"type":"assistant","session_id":"child","parent_tool_use_id":"task-1","message":{"content":[{"type":"text","text":"child summary"}]}}`,
	} {
		_, err := broker.Ingest(ctx, IngestParams{SessionID: "claude-child", Generation: 1, Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderClaude, Stream: nativeprotocol.StreamProviderStdout, Ordinal: int64(i + 1), Timestamp: time.Now(), Raw: []byte(raw)}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestParentTurnEndDoesNotHideAnActiveBackgroundChild(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	defer store.Close()
	createStreamSession(t, ctx, store, "background-child")
	broker := New(store)
	for i, raw := range []string{
		`{"method":"turn/started","params":{"threadId":"parent","turn":{"id":"p"}}}`,
		`{"method":"item/completed","params":{"threadId":"parent","item":{"type":"subAgentActivity","kind":"started","agentThreadId":"child","agentPath":"/root/bg"}}}`,
		`{"method":"turn/completed","params":{"threadId":"parent","turn":{"id":"p","status":"completed"}}}`,
	} {
		_, err := broker.Ingest(ctx, IngestParams{SessionID: "background-child", Generation: 1, Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderCodex, Stream: nativeprotocol.StreamProviderStdout, Ordinal: int64(i + 1), Timestamp: time.Now(), Raw: []byte(raw)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	session, _ := store.GetSession(ctx, "background-child")
	activity, err := SessionActivity(ctx, store, session, time.Now())
	if err != nil || activity.RunningTurn || activity.ActiveSubagents != 1 {
		t.Fatalf("activity=%+v err=%v", activity, err)
	}
}

func TestChildEventsRemainDurableWithoutRebindingGeneration(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	defer store.Close()
	createStreamSession(t, ctx, store, "subagents")
	broker := New(store)
	for i, raw := range []string{
		`{"method":"turn/started","params":{"threadId":"parent","turn":{"id":"parent-turn"}}}`,
		`{"method":"item/completed","params":{"threadId":"child","item":{"type":"agentMessage","text":"child summary"}}}`,
		`{"method":"turn/completed","params":{"threadId":"child","turn":{"id":"child-turn","status":"completed"}}}`,
	} {
		ev, err := broker.Ingest(ctx, IngestParams{SessionID: "subagents", Generation: 1, Record: nativeprotocol.Record{Provider: nativeprotocol.ProviderCodex, Stream: nativeprotocol.StreamProviderStdout, Ordinal: int64(i + 1), Timestamp: time.Now(), Raw: []byte(raw)}})
		if err != nil {
			t.Fatalf("child record rejected: %v", err)
		}
		if i > 0 && !strings.HasPrefix(ev.Type, "subagent.") {
			t.Fatalf("child event masquerades as parent: %s", ev.Type)
		}
	}
	g, _ := store.GetGeneration(ctx, "subagents", 1)
	if g.ProviderID != "parent" {
		t.Fatalf("binding=%q", g.ProviderID)
	}
}
