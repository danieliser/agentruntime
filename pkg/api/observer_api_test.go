package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/observer"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type fakeObserverService struct {
	statuses []observer.PluginStatus
	links    map[string]observer.TraceLink
	readyErr error
	policy   observer.Policy
}

func (fake *fakeObserverService) Status() []observer.PluginStatus { return fake.statuses }
func (fake *fakeObserverService) TraceLink(plugin, sessionID string) (observer.TraceLink, bool) {
	link, ok := fake.links[plugin+"/"+sessionID]
	return link, ok
}
func (fake *fakeObserverService) RequireHealthy(string) error { return fake.readyErr }
func (fake *fakeObserverService) Policy(string) (observer.Policy, bool) {
	if fake.policy == "" {
		return observer.PolicyBestEffort, true
	}
	return fake.policy, true
}

func TestObserverAPIExposesHealthAndSessionTraceLink(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := &fakeObserverService{
		statuses: []observer.PluginStatus{{Name: "opentraces", Version: "0.9.0", Policy: observer.PolicyBestEffort, State: observer.HealthHealthy}},
		links: map[string]observer.TraceLink{"opentraces/session-1": {
			Plugin: "opentraces", SessionID: "session-1", TraceID: "851ad0da-3f90-4ea8-9094-9b644d1913f7", AcknowledgedSequence: 12,
		}},
	}
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store), ObserverService: service,
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/api/v1/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var statuses struct {
		Data []observer.PluginStatus `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&statuses); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(statuses.Data) != 1 || statuses.Data[0].State != observer.HealthHealthy {
		t.Fatalf("plugin status = %d %+v", response.StatusCode, statuses.Data)
	}

	linkResponse, err := http.Get(httpServer.URL + "/api/v1/sessions/session-1/traces")
	if err != nil {
		t.Fatal(err)
	}
	defer linkResponse.Body.Close()
	var links struct {
		Data []observer.TraceLink `json:"data"`
	}
	if err := json.NewDecoder(linkResponse.Body).Decode(&links); err != nil {
		t.Fatal(err)
	}
	if len(links.Data) != 1 || links.Data[0].AcknowledgedSequence != 12 {
		t.Fatalf("trace links = %+v", links.Data)
	}
}

func TestRequiredObserverRejectsFirstAdmissionButNotIdempotentLookup(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := &fakeObserverService{readyErr: errors.New("adapter down")}
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store), ObserverService: service,
	})
	request := SessionRequest{
		IdempotencyKey: "required-job", Agent: "claude", Prompt: "test",
		Trace: &TraceConfig{Plugin: "opentraces", Policy: "required"},
	}
	if _, err := server.admitV1Session(context.Background(), request, "local"); !durable.IsCode(err, durable.CodeInvalidState) {
		t.Fatalf("required admission error = %v", err)
	}
	if _, err := store.GetSessionByIdempotencyKey(context.Background(), request.IdempotencyKey); !durable.IsCode(err, durable.CodeNotFound) {
		t.Fatalf("failed admission persisted a session: %v", err)
	}

	service.readyErr = nil
	created, err := server.admitV1Session(context.Background(), request, "local")
	if err != nil || !created.Created {
		t.Fatalf("healthy required admission = %+v err=%v", created, err)
	}
	service.readyErr = errors.New("adapter failed later")
	repeated, err := server.admitV1Session(context.Background(), request, "local")
	if err != nil || repeated.Created || repeated.Session.ID != created.Session.ID {
		t.Fatalf("idempotent lookup after adapter failure = %+v err=%v", repeated, err)
	}
}

func TestConfiguredRequiredPolicyIsTheCallerDefault(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := &fakeObserverService{readyErr: errors.New("adapter down"), policy: observer.PolicyRequired}
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		DataDir: t.TempDir(), DurableStore: store, EventBroker: eventstream.New(store), ObserverService: service,
	})
	request := SessionRequest{
		IdempotencyKey: "default-required-job", Agent: "claude", Prompt: "test",
		Trace: &TraceConfig{Plugin: "opentraces"},
	}
	if _, err := server.admitV1Session(context.Background(), request, "local"); !durable.IsCode(err, durable.CodeInvalidState) {
		t.Fatalf("configured required admission error = %v", err)
	}
}
