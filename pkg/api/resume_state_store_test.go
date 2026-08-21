package api

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestResumeStateStoreRoundTrip(t *testing.T) {
	store := newResumeStateStore(t.TempDir())
	stateTar := testProviderStateTar(t, "rollout/session.jsonl", []byte("provider conversation"))
	manifest := portableResumeManifest{
		SchemaVersion: "1.0", Agent: "codex", ProviderSessionID: "thread-123",
		SourceSessionID: "session-123", ProviderStateTarget: "/home/agent/.codex/sessions",
		ImageReference: "agentruntime-agent-codex:2.3.0", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC),
	}
	id, saved, err := store.Export(context.Background(), manifest, func(_ context.Context, writer io.Writer) error {
		_, err := writer.Write(stateTar)
		return err
	})
	if err != nil {
		t.Fatalf("export portable state: %v", err)
	}
	if len(id) != 64 || saved.ProviderStateSHA256 == "" {
		t.Fatalf("portable state id=%q manifest=%+v", id, saved)
	}
	loaded, err := store.Manifest(id)
	if err != nil {
		t.Fatalf("load portable manifest: %v", err)
	}
	if loaded.Agent != "codex" || loaded.ProviderSessionID != "thread-123" || loaded.ProviderStateSHA256 != saved.ProviderStateSHA256 {
		t.Fatalf("loaded manifest = %+v", loaded)
	}
	var imported bytes.Buffer
	if err := store.Import(context.Background(), id, func(_ context.Context, reader io.Reader) error {
		_, err := io.Copy(&imported, reader)
		return err
	}); err != nil {
		t.Fatalf("import portable state: %v", err)
	}
	if !bytes.Equal(imported.Bytes(), stateTar) {
		t.Fatalf("imported provider state differs: got=%d want=%d", imported.Len(), len(stateTar))
	}
}

func TestResumeStateStoreRejectsUnsafeTarMember(t *testing.T) {
	store := newResumeStateStore(t.TempDir())
	stateTar := testProviderStateTar(t, "../../host-escape", []byte("no"))
	_, _, err := store.Export(context.Background(), portableResumeManifest{
		SchemaVersion: "1.0", Agent: "claude", ProviderSessionID: "claude-123",
		SourceSessionID: "session-123", ProviderStateTarget: "/home/agent/.claude/projects",
		ImageReference: "agentruntime-agent-claude:2.3.0", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: time.Now().UTC(),
	}, func(_ context.Context, writer io.Writer) error {
		_, err := writer.Write(stateTar)
		return err
	})
	if err == nil {
		t.Fatal("unsafe provider-state tar was accepted")
	}
}

func TestResumeStateStoreUploadIsContentAddressed(t *testing.T) {
	source := newResumeStateStore(t.TempDir())
	stateTar := testProviderStateTar(t, "session.jsonl", []byte("portable"))
	id, _, err := source.Export(context.Background(), portableResumeManifest{
		SchemaVersion: "1.0", Agent: "codex", ProviderSessionID: "thread-upload",
		SourceSessionID: "session-upload", ProviderStateTarget: "/home/agent/.codex/sessions",
		ImageReference: "agent:codex", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedAt: time.Now().UTC(),
	}, func(_ context.Context, writer io.Writer) error {
		_, err := writer.Write(stateTar)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := source.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	destination := newResumeStateStore(t.TempDir())
	uploadedID, uploaded, err := destination.Upload(context.Background(), bundle)
	if err != nil {
		t.Fatalf("upload portable resume state: %v", err)
	}
	if uploadedID != id || uploaded.ProviderSessionID != "thread-upload" {
		t.Fatalf("uploaded id=%q manifest=%+v, want id=%q", uploadedID, uploaded, id)
	}
}

func TestResolvePortableResumeStateRequiresMatchingDockerProvider(t *testing.T) {
	stateStore := newResumeStateStore(t.TempDir())
	stateTar := testProviderStateTar(t, "session.jsonl", []byte("portable"))
	id, _, err := stateStore.Export(context.Background(), portableResumeManifest{
		SchemaVersion: "1.0", Agent: "codex", ProviderSessionID: "thread-resolve",
		SourceSessionID: "session-resolve", ProviderStateTarget: "/home/agent/.codex/sessions",
		ImageReference: "agent:codex", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedAt: time.Now().UTC(),
	}, func(_ context.Context, writer io.Writer) error {
		_, err := writer.Write(stateTar)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{resumeStates: stateStore}
	resolved, err := server.resolvePortableResumeState(SessionRequest{Agent: "codex", Runtime: "docker", ResumeStateID: id})
	if err != nil {
		t.Fatalf("resolve portable resume: %v", err)
	}
	if resolved.ProviderID != "thread-resolve" || resolved.PortableStateID != id {
		t.Fatalf("resolved portable state = %+v", resolved)
	}
	if _, err := server.resolvePortableResumeState(SessionRequest{Agent: "claude", Runtime: "docker", ResumeStateID: id}); err == nil {
		t.Fatal("cross-provider portable state was accepted")
	}
	if _, err := server.resolvePortableResumeState(SessionRequest{Agent: "codex", Runtime: "local", ResumeStateID: id}); err == nil {
		t.Fatal("local portable state import was accepted")
	}
	if _, err := server.resolvePortableResumeState(SessionRequest{Agent: "codex", Runtime: "docker", ResumeStateID: id, ResumeSession: "other"}); err == nil {
		t.Fatal("ambiguous resume sources were accepted")
	}
}

func testProviderStateTar(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
