package durable_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
)

// DUR-101 through DUR-105: every durable Store implementation must pass this
// suite. The SQLite implementation will be added as a second factory after G1.
func TestMemoryStoreContract(t *testing.T) {
	t.Parallel()
	runStoreContract(t, func(t *testing.T) durable.Store {
		t.Helper()
		store := memory.New()
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("close store: %v", err)
			}
		})
		return store
	})
}

type storeFactory func(t *testing.T) durable.Store

func runStoreContract(t *testing.T, factory storeFactory) {
	t.Helper()

	t.Run("idempotent concurrent session creation", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		params := durable.CreateSessionParams{
			SessionID:       "session-idempotent",
			IdempotencyKey:  "job-idempotent",
			RequestHash:     "sha256:request-a",
			RequestManifest: json.RawMessage(`{"agent":"claude","runtime":"docker"}`),
			SecretGrants:    []string{"ANTHROPIC_API_KEY"},
			Agent:           "claude",
			Runtime:         "docker",
			CreatedAt:       time.Unix(100, 0).UTC(),
		}

		const workers = 32
		results := make(chan durable.CreateSessionResult, workers)
		errors := make(chan error, workers)
		var wait sync.WaitGroup
		for range workers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				result, err := store.CreateSession(ctx, params)
				if err != nil {
					errors <- err
					return
				}
				results <- result
			}()
		}
		wait.Wait()
		close(results)
		close(errors)

		for err := range errors {
			t.Errorf("concurrent create: %v", err)
		}
		created := 0
		for result := range results {
			if result.Session.ID != params.SessionID {
				t.Errorf("session ID = %q, want %q", result.Session.ID, params.SessionID)
			}
			if result.Created {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("created count = %d, want 1", created)
		}

		byKey, err := store.GetSessionByIdempotencyKey(ctx, params.IdempotencyKey)
		if err != nil {
			t.Fatalf("get by idempotency key: %v", err)
		}
		if byKey.ID != params.SessionID {
			t.Fatalf("stored session ID = %q, want %q", byKey.ID, params.SessionID)
		}
		byKey.RequestManifest[0] = '['
		byKey.SecretGrants[0] = "MUTATED"
		unchanged, err := store.GetSession(ctx, params.SessionID)
		if err != nil {
			t.Fatalf("get immutable session copy: %v", err)
		}
		if string(unchanged.RequestManifest) != string(params.RequestManifest) || unchanged.SecretGrants[0] != "ANTHROPIC_API_KEY" {
			t.Fatalf("caller mutated stored session: manifest=%s grants=%v", unchanged.RequestManifest, unchanged.SecretGrants)
		}
	})

	t.Run("idempotency key rejects a different request hash", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		first := durable.CreateSessionParams{
			SessionID:       "session-conflict",
			IdempotencyKey:  "job-conflict",
			RequestHash:     "sha256:request-a",
			RequestManifest: json.RawMessage(`{"agent":"codex","runtime":"docker"}`),
			Agent:           "codex",
			Runtime:         "docker",
			CreatedAt:       time.Unix(200, 0).UTC(),
		}
		if _, err := store.CreateSession(ctx, first); err != nil {
			t.Fatalf("first create: %v", err)
		}
		second := first
		second.SessionID = "session-must-not-exist"
		second.RequestHash = "sha256:request-b"
		_, err := store.CreateSession(ctx, second)
		if !durable.IsCode(err, durable.CodeIdempotencyConflict) {
			t.Fatalf("error = %v, want code %s", err, durable.CodeIdempotencyConflict)
		}
		if _, err := store.GetSession(ctx, second.SessionID); !durable.IsCode(err, durable.CodeNotFound) {
			t.Fatalf("conflicting session was created: %v", err)
		}
	})

	t.Run("session requires a reconstructable request manifest", func(t *testing.T) {
		store := factory(t)
		_, err := store.CreateSession(context.Background(), durable.CreateSessionParams{
			SessionID:      "session-no-manifest",
			IdempotencyKey: "job-no-manifest",
			RequestHash:    "sha256:no-manifest",
			Agent:          "claude",
			Runtime:        "docker",
			CreatedAt:      time.Unix(250, 0).UTC(),
		})
		if !durable.IsCode(err, durable.CodeInvalidArgument) {
			t.Fatalf("missing manifest error = %v, want code %s", err, durable.CodeInvalidArgument)
		}
	})

	t.Run("generation numbers are monotonic", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		createContractSession(t, ctx, store, "session-generation")

		first, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
			SessionID:      "session-generation",
			Runtime:        "docker",
			ContainerID:    "container-one",
			ImageReference: "agent:fixture",
			ImageDigest:    "sha256:image-one",
			SandboxProfile: "sandbox-v1",
			CreatedAt:      time.Unix(300, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("create first generation: %v", err)
		}
		if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{
			SessionID:  "session-generation",
			Generation: first.Number,
			From:       durable.GenerationStarting,
			To:         durable.GenerationLost,
			At:         time.Unix(300, 500).UTC(),
		}); err != nil {
			t.Fatalf("close first generation: %v", err)
		}
		second, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
			SessionID:      "session-generation",
			Runtime:        "docker",
			ContainerID:    "container-two",
			ImageReference: "agent:fixture",
			ImageDigest:    "sha256:image-two",
			SandboxProfile: "sandbox-v1",
			CreatedAt:      time.Unix(301, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("create second generation: %v", err)
		}
		if first.Number != 1 || second.Number != 2 {
			t.Fatalf("generation numbers = %d, %d; want 1, 2", first.Number, second.Number)
		}
	})

	t.Run("concurrent event append allocates a contiguous sequence", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		createContractSession(t, ctx, store, "session-events")
		createContractGeneration(t, ctx, store, "session-events")

		const eventCount = 100
		sequences := make(chan int64, eventCount)
		errors := make(chan error, eventCount)
		var wait sync.WaitGroup
		for index := range eventCount {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				raw := []byte(fmt.Sprintf(`{"index":%d}`, index))
				result, err := store.AppendEvent(ctx, durable.AppendEventParams{
					EventID:       fmt.Sprintf("event-%03d", index),
					SessionID:     "session-events",
					Generation:    1,
					SchemaVersion: "1.0",
					Timestamp:     time.Unix(400, int64(index)).UTC(),
					Type:          "content.delta",
					Stream:        durable.StreamProviderStdout,
					Payload:       json.RawMessage(raw),
					Raw:           raw,
				})
				if err != nil {
					errors <- err
					return
				}
				sequences <- result.Event.Sequence
			}(index)
		}
		wait.Wait()
		close(sequences)
		close(errors)

		for err := range errors {
			t.Errorf("append event: %v", err)
		}
		got := make([]int, 0, eventCount)
		for sequence := range sequences {
			got = append(got, int(sequence))
		}
		sort.Ints(got)
		if len(got) != eventCount {
			t.Fatalf("sequence count = %d, want %d", len(got), eventCount)
		}
		for index, sequence := range got {
			if want := index + 1; sequence != want {
				t.Fatalf("sequence[%d] = %d, want %d", index, sequence, want)
			}
		}

		page, err := store.ListEvents(ctx, durable.EventQuery{SessionID: "session-events", AfterSequence: 90, Limit: 20})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		if len(page.Events) != 10 || page.Events[0].Sequence != 91 || page.LastSequence != 100 {
			t.Fatalf("unexpected event page: len=%d first=%d last=%d", len(page.Events), page.Events[0].Sequence, page.LastSequence)
		}
	})

	t.Run("duplicate event ID is idempotent but immutable", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		createContractSession(t, ctx, store, "session-event-dedupe")
		createContractGeneration(t, ctx, store, "session-event-dedupe")

		params := durable.AppendEventParams{
			EventID:       "event-stable",
			SessionID:     "session-event-dedupe",
			Generation:    1,
			SchemaVersion: "1.0",
			Timestamp:     time.Unix(500, 0).UTC(),
			Type:          "content.delta",
			Stream:        durable.StreamProviderStdout,
			Payload:       json.RawMessage(`{"text":"same"}`),
			Raw:           []byte(`{"native":"same"}`),
		}
		first, err := store.AppendEvent(ctx, params)
		if err != nil {
			t.Fatalf("first append: %v", err)
		}
		second, err := store.AppendEvent(ctx, params)
		if err != nil {
			t.Fatalf("duplicate append: %v", err)
		}
		if !first.Created || second.Created || first.Event.Sequence != second.Event.Sequence {
			t.Fatalf("idempotent append results = %+v then %+v", first, second)
		}

		changed := params
		changed.Raw = []byte(`{"native":"changed"}`)
		_, err = store.AppendEvent(ctx, changed)
		if !durable.IsCode(err, durable.CodeImmutableConflict) {
			t.Fatalf("changed duplicate error = %v, want code %s", err, durable.CodeImmutableConflict)
		}
	})

	t.Run("late event timestamps do not move session time backward", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		createContractSession(t, ctx, store, "session-event-time")
		createContractGeneration(t, ctx, store, "session-event-time")
		for index, timestamp := range []time.Time{time.Unix(700, 0).UTC(), time.Unix(699, 0).UTC()} {
			raw := []byte(fmt.Sprintf(`{"index":%d}`, index))
			if _, err := store.AppendEvent(ctx, durable.AppendEventParams{
				EventID:       fmt.Sprintf("event-time-%d", index),
				SessionID:     "session-event-time",
				Generation:    1,
				SchemaVersion: "1.0",
				Timestamp:     timestamp,
				Type:          "content.delta",
				Stream:        durable.StreamProviderStdout,
				Payload:       json.RawMessage(raw),
				Raw:           raw,
			}); err != nil {
				t.Fatalf("append event %d: %v", index, err)
			}
		}
		session, err := store.GetSession(ctx, "session-event-time")
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if want := time.Unix(700, 0).UTC(); !session.UpdatedAt.Equal(want) {
			t.Fatalf("updated at = %s, want %s", session.UpdatedAt, want)
		}
	})

	t.Run("completed requires a running session", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		createContractSession(t, ctx, store, "session-invalid-complete")
		createContractGeneration(t, ctx, store, "session-invalid-complete")
		exitCode := 0
		_, err := store.FinalizeSession(ctx, durable.FinalizeSessionParams{
			From:           durable.StateCreated,
			GenerationFrom: durable.GenerationStarting,
			GenerationTo:   durable.GenerationExited,
			Receipt: durable.TerminalReceipt{
				SessionID:    "session-invalid-complete",
				Generation:   1,
				State:        durable.StateCompleted,
				ExitCode:     &exitCode,
				StartedAt:    time.Unix(750, 0).UTC(),
				EndedAt:      time.Unix(751, 0).UTC(),
				OutputHash:   "sha256:output",
				LastSequence: 0,
			},
		})
		if !durable.IsCode(err, durable.CodeInvalidState) {
			t.Fatalf("invalid completion error = %v, want code %s", err, durable.CodeInvalidState)
		}
	})

	t.Run("terminal session and receipt are immutable", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		createContractSession(t, ctx, store, "session-terminal")
		createContractGeneration(t, ctx, store, "session-terminal")

		if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
			SessionID: "session-terminal",
			From:      durable.StateCreated,
			To:        durable.StateStarting,
			At:        time.Unix(600, 0).UTC(),
		}); err != nil {
			t.Fatalf("created to starting: %v", err)
		}
		if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
			SessionID: "session-terminal",
			From:      durable.StateStarting,
			To:        durable.StateRunning,
			At:        time.Unix(601, 0).UTC(),
		}); err != nil {
			t.Fatalf("starting to running: %v", err)
		}
		exitCode := 0
		receipt := durable.TerminalReceipt{
			SessionID:    "session-terminal",
			Generation:   1,
			State:        durable.StateCompleted,
			ExitCode:     &exitCode,
			StartedAt:    time.Unix(601, 0).UTC(),
			EndedAt:      time.Unix(602, 0).UTC(),
			OutputHash:   "sha256:output",
			ArtifactHash: "sha256:artifacts",
			LastSequence: 0,
		}
		first, err := store.FinalizeSession(ctx, durable.FinalizeSessionParams{
			From:           durable.StateRunning,
			GenerationFrom: durable.GenerationStarting,
			GenerationTo:   durable.GenerationExited,
			Receipt:        receipt,
		})
		if err != nil {
			t.Fatalf("finalize session: %v", err)
		}
		if !first.Session.State.Terminal() {
			t.Fatalf("state %q must be terminal", first.Session.State)
		}
		second, err := store.FinalizeSession(ctx, durable.FinalizeSessionParams{
			From:           durable.StateRunning,
			GenerationFrom: durable.GenerationStarting,
			GenerationTo:   durable.GenerationExited,
			Receipt:        receipt,
		})
		if err != nil {
			t.Fatalf("repeat finalize: %v", err)
		}
		if !first.Created || second.Created {
			t.Fatalf("idempotent finalize results = %+v then %+v", first, second)
		}

		changed := receipt
		changed.OutputHash = "sha256:different"
		_, err = store.FinalizeSession(ctx, durable.FinalizeSessionParams{
			From:           durable.StateRunning,
			GenerationFrom: durable.GenerationStarting,
			GenerationTo:   durable.GenerationExited,
			Receipt:        changed,
		})
		if !durable.IsCode(err, durable.CodeImmutableConflict) {
			t.Fatalf("changed receipt error = %v, want code %s", err, durable.CodeImmutableConflict)
		}
		_, err = store.TransitionSession(ctx, durable.TransitionSessionParams{
			SessionID: "session-terminal",
			From:      durable.StateCompleted,
			To:        durable.StateRunning,
			At:        time.Unix(603, 0).UTC(),
		})
		if !durable.IsCode(err, durable.CodeInvalidState) {
			t.Fatalf("terminal transition error = %v, want code %s", err, durable.CodeInvalidState)
		}
		_, err = store.AppendEvent(ctx, durable.AppendEventParams{
			EventID:       "event-after-terminal",
			SessionID:     "session-terminal",
			Generation:    1,
			SchemaVersion: "1.0",
			Timestamp:     time.Unix(604, 0).UTC(),
			Type:          "content.delta",
			Stream:        durable.StreamProviderStdout,
			Payload:       json.RawMessage(`{"text":"too late"}`),
			Raw:           []byte(`{"native":"too late"}`),
		})
		if !durable.IsCode(err, durable.CodeInvalidState) {
			t.Fatalf("post-terminal append error = %v, want code %s", err, durable.CodeInvalidState)
		}
	})

	t.Run("closed store fails structurally", func(t *testing.T) {
		store := memory.New()
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
		_, err := store.GetSession(context.Background(), "missing")
		if !durable.IsCode(err, durable.CodeStoreClosed) {
			t.Fatalf("closed store error = %v, want code %s", err, durable.CodeStoreClosed)
		}
	})
}
