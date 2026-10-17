package durable_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/durable/sqlite"
)

func TestCompletedSessionContinuationPreservesGenerationReceipts(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store durable.Store = memory.New()
			if backend == "sqlite" {
				var err error
				store, err = sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			createContractSession(t, ctx, store, "continued")
			from := durable.StateCreated
			for number := int64(1); number <= 2; number++ {
				if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "continued", From: from, To: durable.StateStarting}); err != nil {
					t.Fatalf("start generation %d: %v", number, err)
				}
				generation, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "continued", Runtime: "docker", ContainerID: fmt.Sprintf("container-%d", number)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{SessionID: "continued", Generation: generation.Number, From: durable.GenerationStarting, To: durable.GenerationRunning}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: "continued", From: durable.StateStarting, To: durable.StateRunning}); err != nil {
					t.Fatal(err)
				}
				at := time.Now().UTC()
				_, err = store.FinalizeSession(ctx, durable.FinalizeSessionParams{From: durable.StateRunning, GenerationFrom: durable.GenerationRunning, GenerationTo: durable.GenerationExited, Receipt: durable.TerminalReceipt{SessionID: "continued", Generation: number, State: durable.StateCompleted, StartedAt: generation.CreatedAt, EndedAt: at, OutputHash: fmt.Sprintf("hash-%d", number)}})
				if err != nil {
					t.Fatalf("finalize generation %d: %v", number, err)
				}
				from = durable.StateCompleted
			}
			latest, err := store.GetTerminalReceipt(ctx, "continued")
			if err != nil || latest.Generation != 2 {
				t.Fatalf("latest=%+v err=%v", latest, err)
			}
			first, err := store.GetGenerationReceipt(ctx, "continued", 1)
			if err != nil || first.OutputHash != "hash-1" {
				t.Fatalf("first receipt=%+v err=%v", first, err)
			}
		})
	}
}
