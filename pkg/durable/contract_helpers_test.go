package durable_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func createContractSession(t *testing.T, ctx context.Context, store durable.Store, sessionID string) durable.Session {
	t.Helper()
	result, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID:       sessionID,
		IdempotencyKey:  "job-" + sessionID,
		RequestHash:     "sha256:" + sessionID,
		RequestManifest: json.RawMessage(`{"agent":"claude","runtime":"docker"}`),
		Agent:           "claude",
		Runtime:         "docker",
		CreatedAt:       time.Unix(50, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return result.Session
}

func createContractGeneration(t *testing.T, ctx context.Context, store durable.Store, sessionID string) durable.Generation {
	t.Helper()
	generation, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID:      sessionID,
		Runtime:        "docker",
		ContainerID:    "container-" + sessionID,
		ImageReference: "agent:fixture",
		ImageDigest:    "sha256:image",
		SandboxProfile: "sandbox-v1",
		CreatedAt:      time.Unix(51, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("create generation: %v", err)
	}
	return generation
}
