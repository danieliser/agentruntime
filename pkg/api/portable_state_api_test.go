package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablememory "github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestPortableResumeStateAPIRoundTrip(t *testing.T) {
	providerState := testProviderStateTar(t, "rollout/thread.jsonl", []byte("portable conversation"))
	rt := &portableStateAPIRuntime{providerState: providerState}
	store := durablememory.New()
	manager := session.NewManager()
	server := NewServer(manager, rt, agent.NewRegistry(), ServerConfig{
		DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store,
	})
	t.Cleanup(manager.ShutdownAll)
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)

	sessionID := createPortableStateTestSession(t, store)
	active := newActiveNativeSession(nil)
	active.lease = newNativeContainerLease(time.Hour, nil)
	active.lease.TurnCompleted()
	server.native[sessionID] = active

	response, err := http.Post(httpServer.URL+"/api/v1/sessions/"+sessionID+"/resume-state", "application/json", nil)
	if err != nil {
		t.Fatalf("export portable state: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("export status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	var created struct {
		Data v1ResumeStateData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if len(created.Data.ResumeStateID) != 64 || created.Data.ProviderSessionID != "provider-thread" {
		t.Fatalf("created portable state = %+v", created.Data)
	}
	if rt.exportAgent != "codex" || rt.exportVolume != "agentruntime-vol-"+sessionID {
		t.Fatalf("export target agent=%q volume=%q", rt.exportAgent, rt.exportVolume)
	}

	latest, err := http.Get(httpServer.URL + "/api/v1/sessions/" + sessionID + "/resume-state")
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Body.Close()
	if latest.StatusCode != http.StatusOK {
		t.Fatalf("latest status=%d body=%s", latest.StatusCode, readResponse(t, latest))
	}
	var latestEnvelope struct {
		Data v1ResumeStateData `json:"data"`
	}
	if err := json.NewDecoder(latest.Body).Decode(&latestEnvelope); err != nil {
		t.Fatal(err)
	}
	if latestEnvelope.Data.ResumeStateID != created.Data.ResumeStateID {
		t.Fatalf("latest state=%q want=%q", latestEnvelope.Data.ResumeStateID, created.Data.ResumeStateID)
	}

	download, err := http.Get(httpServer.URL + "/api/v1/resume-states/" + created.Data.ResumeStateID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := io.ReadAll(download.Body)
	download.Body.Close()
	if err != nil || download.StatusCode != http.StatusOK || len(bundle) == 0 {
		t.Fatalf("download status=%d bytes=%d err=%v", download.StatusCode, len(bundle), err)
	}
	if got := download.Header.Get("Content-Type"); got != PortableResumeContentType {
		t.Fatalf("download content type=%q", got)
	}

	upload, err := http.Post(httpServer.URL+"/api/v1/resume-states", PortableResumeContentType, bytes.NewReader(bundle))
	if err != nil {
		t.Fatal(err)
	}
	defer upload.Body.Close()
	if upload.StatusCode != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", upload.StatusCode, readResponse(t, upload))
	}
	var uploaded struct {
		Data v1ResumeStateData `json:"data"`
	}
	if err := json.NewDecoder(upload.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.Data.ResumeStateID != created.Data.ResumeStateID {
		t.Fatalf("uploaded state=%q want=%q", uploaded.Data.ResumeStateID, created.Data.ResumeStateID)
	}
}

func TestPortableResumeStateAPIRejectsActiveTurn(t *testing.T) {
	rt := &portableStateAPIRuntime{providerState: testProviderStateTar(t, "thread.jsonl", []byte("state"))}
	store := durablememory.New()
	server := NewServer(session.NewManager(), rt, agent.NewRegistry(), ServerConfig{
		DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store,
	})
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	sessionID := createPortableStateTestSession(t, store)
	active := newActiveNativeSession(nil)
	active.lease = newNativeContainerLease(time.Hour, nil)
	server.native[sessionID] = active

	response, err := http.Post(httpServer.URL+"/api/v1/sessions/"+sessionID+"/resume-state", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("active-turn export status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
}

func TestPortableResumeStateAuthenticatedUploadAllowsBoundedArchiveBody(t *testing.T) {
	source := newResumeStateStore(t.TempDir())
	stateTar := testProviderStateTar(t, "rollout/large.jsonl", bytes.Repeat([]byte("x"), maxRequestBodyBytes+1024))
	id, _, err := source.Export(context.Background(), portableResumeManifest{
		SchemaVersion: portableResumeSchemaVersion, Agent: "codex", ProviderSessionID: "provider-large",
		SourceSessionID: "source-large", ProviderStateTarget: portableProviderTarget("codex"),
		ImageReference: "agentruntime-agent-codex:2.3.0",
		ImageDigest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedAt: time.Now().UTC(),
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
	server := NewServer(session.NewManager(), &portableStateAPIRuntime{}, agent.NewRegistry(), ServerConfig{
		DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"), AuthToken: "test-token",
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/resume-states", bundle)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", PortableResumeContentType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("authenticated large upload status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
}

func TestPortableResumeStateColdImportResumesProviderWithCurrentWorkDir(t *testing.T) {
	stateTar := testProviderStateTar(t, "rollout/thread.jsonl", []byte("portable conversation"))
	rt := &portableImportTestRuntime{Runtime: runtime.NewLocalRuntime()}
	store := durablememory.New()
	manager := session.NewManager()
	registry := agent.NewRegistry()
	registry.Register(&resumingCodexFixtureAgent{})
	dataDir := t.TempDir()
	server := NewServer(manager, rt, registry, ServerConfig{
		DataDir: dataDir, LogDir: filepath.Join(dataDir, "logs"), DurableStore: store,
		EventBroker: eventstream.New(store),
	})
	t.Cleanup(manager.ShutdownAll)
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	stateID, _, err := server.resumeStates.Export(context.Background(), portableResumeManifest{
		SchemaVersion: portableResumeSchemaVersion, Agent: "codex", ProviderSessionID: "provider-original-thread",
		SourceSessionID: "source-session", ProviderStateTarget: portableProviderTarget("codex"),
		ImageReference: "agentruntime-agent-codex:2.3.0", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedAt: time.Now().UTC(),
	}, func(_ context.Context, writer io.Writer) error {
		_, err := writer.Write(stateTar)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	currentWorkDir := t.TempDir()
	response := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "portable-cold-import", "agent": "codex", "runtime": "docker",
		"prompt": "continue", "resume_state_id": stateID, "work_dir": currentWorkDir,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("cold import status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	var created struct {
		Data v1SessionData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)
	rt.mu.Lock()
	imported := append([]byte(nil), rt.imported...)
	importVolume := rt.importVolume
	spawnVolume := rt.spawnVolume
	spawnWorkDir := rt.spawnWorkDir
	rt.mu.Unlock()
	if !bytes.Equal(imported, stateTar) {
		t.Fatalf("imported provider state differs: got=%d want=%d", len(imported), len(stateTar))
	}
	wantVolume := "agentruntime-vol-" + created.Data.SessionID
	if importVolume != wantVolume || spawnVolume != wantVolume {
		t.Fatalf("import volume=%q spawn volume=%q want=%q", importVolume, spawnVolume, wantVolume)
	}
	if spawnWorkDir != currentWorkDir {
		t.Fatalf("spawn work_dir=%q want remapped %q", spawnWorkDir, currentWorkDir)
	}
	generation, err := store.GetGeneration(context.Background(), created.Data.SessionID, 1)
	if err != nil || generation.ProviderID != "provider-original-thread" {
		t.Fatalf("resumed generation=%+v err=%v", generation, err)
	}
}

func createPortableStateTestSession(t *testing.T, store durable.Store) string {
	t.Helper()
	now := time.Now().UTC()
	created, err := store.CreateSession(context.Background(), durable.CreateSessionParams{
		SessionID: "11111111-1111-4111-8111-111111111111", IdempotencyKey: "portable-api",
		RequestHash: "sha256:request", RequestManifest: json.RawMessage(`{"agent":"codex","runtime":"docker","persist_session":true}`),
		Agent: "codex", Runtime: "docker", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionSession(context.Background(), durable.TransitionSessionParams{SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	generation, err := store.CreateGeneration(context.Background(), durable.CreateGenerationParams{
		SessionID: created.Session.ID, Runtime: "docker", ContainerID: "container-portable",
		ImageReference: "agentruntime-agent-codex:2.3.0", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ProviderID: "provider-thread", CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionGeneration(context.Background(), durable.TransitionGenerationParams{SessionID: created.Session.ID, Generation: generation.Number, From: durable.GenerationStarting, To: durable.GenerationRunning, At: now.Add(3 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionSession(context.Background(), durable.TransitionSessionParams{SessionID: created.Session.ID, From: durable.StateStarting, To: durable.StateRunning, At: now.Add(4 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	return created.Session.ID
}

type portableStateAPIRuntime struct {
	providerState []byte
	exportAgent   string
	exportVolume  string
}

func (*portableStateAPIRuntime) Name() string { return "docker" }
func (*portableStateAPIRuntime) Spawn(context.Context, runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	return nil, nil
}
func (*portableStateAPIRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) {
	return nil, nil
}
func (*portableStateAPIRuntime) Cleanup(context.Context) error { return nil }
func (rt *portableStateAPIRuntime) ExportProviderState(_ context.Context, agent, volume string, writer io.Writer) error {
	rt.exportAgent = agent
	rt.exportVolume = volume
	_, err := writer.Write(rt.providerState)
	return err
}
func (*portableStateAPIRuntime) ImportProviderState(context.Context, string, string, io.Reader) error {
	return nil
}
func (*portableStateAPIRuntime) RemoveProviderState(context.Context, string) error { return nil }

type portableImportTestRuntime struct {
	runtime.Runtime
	mu           sync.Mutex
	imported     []byte
	importVolume string
	spawnVolume  string
	spawnWorkDir string
	exportState  []byte
}

func (*portableImportTestRuntime) Name() string { return "docker" }
func (rt *portableImportTestRuntime) Spawn(ctx context.Context, config runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	rt.mu.Lock()
	rt.spawnVolume = config.VolumeName
	rt.spawnWorkDir = config.WorkDir
	rt.mu.Unlock()
	handle, err := rt.Runtime.Spawn(ctx, config)
	if err != nil {
		return nil, err
	}
	return &portableImportTestHandle{ProcessHandle: handle}, nil
}
func (rt *portableImportTestRuntime) ImportProviderState(_ context.Context, _ string, volume string, reader io.Reader) error {
	contents, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.imported = contents
	rt.importVolume = volume
	rt.mu.Unlock()
	return nil
}
func (rt *portableImportTestRuntime) ExportProviderState(_ context.Context, _ string, _ string, writer io.Writer) error {
	_, err := writer.Write(rt.exportState)
	return err
}
func (*portableImportTestRuntime) RemoveProviderState(context.Context, string) error { return nil }

type portableImportTestHandle struct{ runtime.ProcessHandle }

func (*portableImportTestHandle) NativeStdio() bool { return true }
func (*portableImportTestHandle) RuntimeImageDigest() string {
	return "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}
func (*portableImportTestHandle) RuntimeImageReference() string {
	return "agentruntime-agent-codex:2.3.0"
}
