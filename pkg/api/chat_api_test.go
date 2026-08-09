package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/chat"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablememory "github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// --- chat test helpers ---

// newChatTestServer creates a test server with the chat subsystem wired up.
// Uses a stub spawner that creates fake sessions.
func newChatTestServer(t *testing.T) (*httptest.Server, *Server, *chat.Manager, *stubSpawner) {
	t.Helper()
	rt := newFakeRuntime(t)
	reg := agent.NewRegistry()
	reg.Register(&echoAgent{})
	reg.Register(&catAgent{})
	reg.Register(&sleepAgent{})

	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logdir: %v", err)
	}

	sessMgr := session.NewManager()

	chatReg, err := chat.NewRegistry(dataDir)
	if err != nil {
		t.Fatalf("new chat registry: %v", err)
	}

	spawner := &stubSpawner{sessions: sessMgr}
	durableStore := durablememory.New()
	t.Cleanup(func() { _ = durableStore.Close() })
	runtimeMap := map[string]string{"test": "test"}
	_ = runtimeMap // just for documentation

	chatMgr := chat.NewManager(
		chatReg,
		sessMgr,
		nil, // no real runtimes needed for API tests
		"test",
		nil, // no volume manager
		spawner,
	)

	srv := NewServer(sessMgr, rt, reg, ServerConfig{
		DataDir:      dataDir,
		LogDir:       logDir,
		ChatRegistry: chatReg,
		ChatManager:  chatMgr,
		DurableStore: durableStore,
		EventBroker:  eventstream.New(durableStore),
	})

	ts := httptest.NewServer(srv.router)
	t.Cleanup(ts.Close)
	return ts, srv, chatMgr, spawner
}

// stubSpawner implements chat.SessionSpawner for tests.
type stubSpawner struct {
	sessions  *session.Manager
	lastReq   apischema.SessionRequest
	failNext  error
	spawnHook func(req apischema.SessionRequest) // called before creating session
}

func (s *stubSpawner) SpawnSession(_ context.Context, req apischema.SessionRequest) (*session.Session, error) {
	s.lastReq = req
	if s.spawnHook != nil {
		s.spawnHook(req)
	}
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return nil, err
	}
	sess := session.NewSession("", req.Agent, "test", req.Tags)
	sess.SetRunning(newFakeHandle())
	if err := s.sessions.Add(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// fakeHandle implements runtime.ProcessHandle for test sessions.
type fakeHandle struct {
	waitCh chan runtime.ExitResult
}

func newFakeHandle() *fakeHandle {
	return &fakeHandle{waitCh: make(chan runtime.ExitResult, 1)}
}

func (h *fakeHandle) PID() int                            { return 99999 }
func (h *fakeHandle) Stdin() io.WriteCloser               { return &nopWriteCloser{} }
func (h *fakeHandle) Stdout() io.ReadCloser               { return nil }
func (h *fakeHandle) Stderr() io.ReadCloser               { return nil }
func (h *fakeHandle) Wait() <-chan runtime.ExitResult     { return h.waitCh }
func (h *fakeHandle) Kill() error                         { return nil }
func (h *fakeHandle) RecoveryInfo() *runtime.RecoveryInfo { return nil }

type nopWriteCloser struct{ bytes.Buffer }

func (n *nopWriteCloser) Close() error { return nil }

// --- helper functions ---

func postJSON(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func patchJSON(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPatch, ts.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new PATCH request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	return resp
}

func httpDelete(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new DELETE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func mustCreateChat(t *testing.T, ts *httptest.Server, name, agentName string) apischema.ChatResponse {
	t.Helper()
	resp := postJSON(t, ts, "/chats", apischema.CreateChatRequest{
		Name: name,
		Config: apischema.ChatAPIConfig{
			Agent: agentName,
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create chat: expected 201, got %d body=%s", resp.StatusCode, string(body))
	}
	var created apischema.ChatResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal chat response: %v", err)
	}
	return created
}

// --- POST /chats ---

func TestCreateChat_201(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	created := mustCreateChat(t, ts, "web-ui", "echo-test")

	if created.Name != "web-ui" {
		t.Fatalf("expected name %q, got %q", "web-ui", created.Name)
	}
	if created.State != "created" {
		t.Fatalf("expected state %q, got %q", "created", created.State)
	}
	if created.Config.Agent != "echo-test" {
		t.Fatalf("expected agent %q, got %q", "echo-test", created.Config.Agent)
	}
	if len(created.SessionChain) != 0 {
		t.Fatalf("expected empty session chain, got %v", created.SessionChain)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("expected non-zero created_at")
	}
}

func TestCreateChat_409_Duplicate(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "dupe-test", "echo-test")

	resp := postJSON(t, ts, "/chats", apischema.CreateChatRequest{
		Name: "dupe-test",
		Config: apischema.ChatAPIConfig{
			Agent: "echo-test",
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestCreateChat_400_MissingAgent(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := postJSON(t, ts, "/chats", apischema.CreateChatRequest{
		Name:   "no-agent",
		Config: apischema.ChatAPIConfig{},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestCreateChat_400_InvalidName(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := postJSON(t, ts, "/chats", apischema.CreateChatRequest{
		Name: "Web-UI",
		Config: apischema.ChatAPIConfig{
			Agent: "echo-test",
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestCreateChat_400_MissingName(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := postJSON(t, ts, "/chats", apischema.CreateChatRequest{
		Config: apischema.ChatAPIConfig{
			Agent: "echo-test",
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, string(body))
	}
}

// --- GET /chats ---

func TestListChats_Empty(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := get(t, ts, "/chats")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var summaries []apischema.ChatSummary
	if err := json.Unmarshal(body, &summaries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(summaries))
	}
}

func TestListChats_Multiple(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "chat-a", "echo-test")
	time.Sleep(time.Millisecond) // ensure ordering
	mustCreateChat(t, ts, "chat-b", "cat-test")

	resp := get(t, ts, "/chats")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var summaries []apischema.ChatSummary
	if err := json.Unmarshal(body, &summaries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 chats, got %d", len(summaries))
	}
	// Sorted by CreatedAt ascending.
	if summaries[0].Name != "chat-a" {
		t.Fatalf("expected first chat %q, got %q", "chat-a", summaries[0].Name)
	}
	if summaries[1].Name != "chat-b" {
		t.Fatalf("expected second chat %q, got %q", "chat-b", summaries[1].Name)
	}
	if summaries[0].SessionCount != 0 {
		t.Fatalf("expected 0 sessions, got %d", summaries[0].SessionCount)
	}
}

// --- GET /chats/:name ---

func TestGetChat_200(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "get-test", "echo-test")

	resp := get(t, ts, "/chats/get-test")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var chatResp apischema.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chatResp.Name != "get-test" {
		t.Fatalf("expected name %q, got %q", "get-test", chatResp.Name)
	}
	if chatResp.State != "created" {
		t.Fatalf("expected state %q, got %q", "created", chatResp.State)
	}
	if chatResp.WSURL != "" {
		t.Fatalf("expected no ws_url for non-running chat, got %q", chatResp.WSURL)
	}
}

func TestGetChat_404(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := get(t, ts, "/chats/nonexistent")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestGetChat_Running_HasWSURL(t *testing.T) {
	ts, _, chatMgr, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "running-ws", "echo-test")

	// Send a message to transition to running.
	_, err := chatMgr.SendMessage("running-ws", "hello")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	resp := get(t, ts, "/chats/running-ws")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var chatResp apischema.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chatResp.State != "running" {
		t.Fatalf("expected state %q, got %q", "running", chatResp.State)
	}
	if !strings.Contains(chatResp.WSURL, "/api/v1/ws/sessions/"+chatResp.CurrentSession+"/events") {
		t.Fatalf("expected durable session event URL, got %q", chatResp.WSURL)
	}
	if chatResp.CurrentSession == "" {
		t.Fatal("expected non-empty current_session for running chat")
	}
	if len(chatResp.SessionChain) != 1 {
		t.Fatalf("expected 1 session in chain, got %d", len(chatResp.SessionChain))
	}
}

// --- POST /chats/:name/messages ---

func TestSendMessage_202_Spawned(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "send-test", "echo-test")

	resp := postJSON(t, ts, "/chats/send-test/messages", apischema.SendMessageRequest{
		Message: "hello world",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", resp.StatusCode, string(body))
	}

	var sendResp apischema.SendMessageResponse
	if err := json.Unmarshal(body, &sendResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sendResp.SessionID == "" {
		t.Fatal("expected non-empty session_id")
	}
	if !sendResp.Spawned {
		t.Fatal("expected spawned=true")
	}
	if sendResp.State != "running" {
		t.Fatalf("expected state %q, got %q", "running", sendResp.State)
	}
	if !strings.Contains(sendResp.WSURL, "/api/v1/ws/sessions/"+sendResp.SessionID+"/events") {
		t.Fatalf("expected durable session event URL, got %q", sendResp.WSURL)
	}
}

func TestSendMessage_202_Stdin(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "stdin-test", "echo-test")

	// First message spawns.
	resp1 := postJSON(t, ts, "/chats/stdin-test/messages", apischema.SendMessageRequest{
		Message: "first",
	})
	readBody(t, resp1)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first message: expected 202, got %d", resp1.StatusCode)
	}

	// Second message injects via stdin (session is running).
	resp2 := postJSON(t, ts, "/chats/stdin-test/messages", apischema.SendMessageRequest{
		Message: "second",
	})
	body2 := readBody(t, resp2)
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("second message: expected 202, got %d body=%s", resp2.StatusCode, string(body2))
	}

	var sendResp apischema.SendMessageResponse
	if err := json.Unmarshal(body2, &sendResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sendResp.Spawned {
		t.Fatal("expected spawned=false for stdin injection")
	}
}

func TestSendMessage_404_NotFound(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := postJSON(t, ts, "/chats/nonexistent/messages", apischema.SendMessageRequest{
		Message: "hello",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestSendMessage_400_EmptyMessage(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "empty-msg", "echo-test")

	resp := postJSON(t, ts, "/chats/empty-msg/messages", apischema.SendMessageRequest{
		Message: "",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, string(body))
	}
}

// --- GET /chats/:name/messages ---

func TestGetChatMessages_200(t *testing.T) {
	ts, srv, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "msg-test", "echo-test")

	sessID := "test-session-001"
	seedDurableChatEvents(t, srv.durableStore, sessID, []string{
		"content.delta", "tool.call", "tool.result", "lifecycle.turn.started", "usage",
	})

	// Manually update the chat record to have this session in the chain.
	rec, _ := srv.chatRegistry.Load("msg-test")
	rec.SessionChain = []string{sessID}
	if err := srv.chatRegistry.Save(rec); err != nil {
		t.Fatalf("save record: %v", err)
	}

	resp := get(t, ts, "/chats/msg-test/messages")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var msgResp apischema.ChatMessagesResponse
	if err := json.Unmarshal(body, &msgResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Should project content/tool/usage — but not unrelated lifecycle events.
	if len(msgResp.Messages) != 4 {
		t.Fatalf("expected 4 messages (excluding progress), got %d", len(msgResp.Messages))
	}
	if msgResp.Messages[0].Type != "agent_message" {
		t.Fatalf("expected first message type %q, got %q", "agent_message", msgResp.Messages[0].Type)
	}
	if msgResp.Messages[0].SessionID != sessID {
		t.Fatalf("expected session_id %q, got %q", sessID, msgResp.Messages[0].SessionID)
	}
	if msgResp.Messages[0].Source != "durable_v1" {
		t.Fatalf("expected durable history source, got %q", msgResp.Messages[0].Source)
	}
}

func (*stubSpawner) SendSessionInput(context.Context, string, string, string, string) error {
	return nil
}

func TestGetChatMessagesMarksLegacyHistoryUnverified(t *testing.T) {
	ts, srv, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "legacy-msg-test", "echo-test")
	sessionID := "legacy-session"
	logPath := session.LogFilePath(srv.logDir, sessionID)
	if err := os.WriteFile(logPath, []byte(`{"type":"agent_message","data":{"text":"old"},"timestamp":1700000000000}`+"\n"), 0o600); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
	rec, _ := srv.chatRegistry.Load("legacy-msg-test")
	rec.SessionChain = []string{sessionID}
	if err := srv.chatRegistry.Save(rec); err != nil {
		t.Fatalf("save legacy chat: %v", err)
	}
	resp := get(t, ts, "/chats/legacy-msg-test/messages")
	defer resp.Body.Close()
	var history apischema.ChatMessagesResponse
	decodeJSON(t, resp.Body, &history)
	if resp.StatusCode != http.StatusOK || len(history.Messages) != 1 || history.Messages[0].Source != "legacy_ndjson_unverified" {
		t.Fatalf("legacy history status=%d response=%+v", resp.StatusCode, history)
	}
}

func TestGetChatMessages_Pagination(t *testing.T) {
	ts, srv, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "page-test", "echo-test")

	sessID := "test-session-page"
	seedDurableChatEvents(t, srv.durableStore, sessID, []string{
		"content.delta", "content.delta", "content.delta", "content.delta", "content.delta",
	})

	rec, _ := srv.chatRegistry.Load("page-test")
	rec.SessionChain = []string{sessID}
	if err := srv.chatRegistry.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Limit to 3.
	resp := get(t, ts, "/chats/page-test/messages?limit=3")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var msgResp apischema.ChatMessagesResponse
	if err := json.Unmarshal(body, &msgResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(msgResp.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgResp.Messages))
	}
	if !msgResp.HasMore {
		t.Fatal("expected has_more=true")
	}
	// Total reflects the count returned in this page (LogReader early-exits at
	// limit+1 for efficiency and does not scan the full history for a count).
	if msgResp.Total != 3 {
		t.Fatalf("expected total=3 (page count), got %d", msgResp.Total)
	}
}

func seedDurableChatEvents(t *testing.T, store durable.Store, sessionID string, eventTypes []string) {
	t.Helper()
	ctx := context.Background()
	createdAt := time.Unix(1_700_000_000, 0).UTC()
	created, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: sessionID, IdempotencyKey: "job-" + sessionID, RequestHash: "sha256:" + sessionID,
		RequestManifest: json.RawMessage(`{"agent":"claude","runtime":"docker"}`),
		Agent:           "claude", Runtime: "docker", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create durable chat session: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
		SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: createdAt.Add(time.Millisecond),
	}); err != nil {
		t.Fatalf("start durable chat session: %v", err)
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: sessionID, Runtime: "docker", ContainerID: "container-" + sessionID, CreatedAt: createdAt.Add(2 * time.Millisecond),
	}); err != nil {
		t.Fatalf("create durable chat generation: %v", err)
	}
	for index, eventType := range eventTypes {
		payload, _ := json.Marshal(map[string]interface{}{"text": "message", "name": "read_file", "index": index})
		if _, err := store.AppendEvent(ctx, durable.AppendEventParams{
			SchemaVersion: "1.0", EventID: fmt.Sprintf("evt-%s-%d", sessionID, index), SessionID: sessionID,
			Generation: 1, Timestamp: createdAt.Add(time.Duration(index+3) * time.Millisecond),
			Type: eventType, Stream: durable.StreamProviderStdout, Payload: payload, Raw: payload,
		}); err != nil {
			t.Fatalf("append durable chat event: %v", err)
		}
	}
}

// --- PATCH /chats/:name/config ---

func TestUpdateChatConfig_200_Idle(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "cfg-test", "echo-test")

	resp := patchJSON(t, ts, "/chats/cfg-test/config", apischema.UpdateChatConfigRequest{
		Config: apischema.ChatAPIConfig{
			Agent:       "cat-test",
			Model:       "opus",
			IdleTimeout: "1h",
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var chatResp apischema.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chatResp.Config.Agent != "cat-test" {
		t.Fatalf("expected agent %q, got %q", "cat-test", chatResp.Config.Agent)
	}
	if chatResp.Config.Model != "opus" {
		t.Fatalf("expected model %q, got %q", "opus", chatResp.Config.Model)
	}
	if chatResp.Config.IdleTimeout != "1h" {
		t.Fatalf("expected idle_timeout %q, got %q", "1h", chatResp.Config.IdleTimeout)
	}
}

func TestUpdateChatConfig_409_Running(t *testing.T) {
	ts, _, chatMgr, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "cfg-running", "echo-test")

	// Transition to running.
	_, err := chatMgr.SendMessage("cfg-running", "hello")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	resp := patchJSON(t, ts, "/chats/cfg-running/config", apischema.UpdateChatConfigRequest{
		Config: apischema.ChatAPIConfig{
			Agent: "cat-test",
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestUpdateChatConfig_404(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := patchJSON(t, ts, "/chats/nonexistent/config", apischema.UpdateChatConfigRequest{
		Config: apischema.ChatAPIConfig{
			Agent: "echo-test",
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestUpdateChatConfig_PartialMerge(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "cfg-merge", "echo-test")

	// PATCH with only model — should merge, not replace
	resp := patchJSON(t, ts, "/chats/cfg-merge/config", apischema.UpdateChatConfigRequest{
		Config: apischema.ChatAPIConfig{Model: "claude-sonnet-4-6"},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
}

// --- DELETE /chats/:name ---

func TestDeleteChat_204(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "del-test", "echo-test")

	resp := httpDelete(t, ts, "/chats/del-test")
	readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify it's gone.
	resp2 := get(t, ts, "/chats/del-test")
	readBody(t, resp2)
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp2.StatusCode)
	}
}

func TestDeleteChat_404(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := httpDelete(t, ts, "/chats/nonexistent")
	readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- retired GET /ws/chats/:name compatibility proxy ---

func TestChatWS_409_NotRunning(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	mustCreateChat(t, ts, "ws-idle", "echo-test")

	// The chat-specific byte-stream proxy is gone; clients use the session's
	// v1 event_stream_url returned by chat inspect/send/attach.
	resp := get(t, ts, "/ws/chats/ws-idle")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestChatWS_404_NotFound(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)
	resp := get(t, ts, "/ws/chats/nonexistent")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", resp.StatusCode, string(body))
	}
}

// --- Full lifecycle: create → send message → get chat → delete ---

// TestCreateChat_MountsPropagated verifies that Mounts in ChatAPIConfig are
// stored in the chat record and forwarded to SpawnSession when a message is sent.
func TestCreateChat_MountsPropagated(t *testing.T) {
	ts, _, chatMgr, sp := newChatTestServer(t)

	mounts := []apischema.Mount{
		{Host: "my-workspace-vol", Container: "/workspace", Type: "volume", Mode: "rw"},
	}
	resp := postJSON(t, ts, "/chats", apischema.CreateChatRequest{
		Name: "mounts-chat",
		Config: apischema.ChatAPIConfig{
			Agent:  "echo-test",
			Mounts: mounts,
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(body))
	}

	// Verify mounts are stored in the config.
	var chatResp apischema.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(chatResp.Config.Mounts) != 1 || chatResp.Config.Mounts[0].Host != "my-workspace-vol" {
		t.Fatalf("expected mounts in response, got %+v", chatResp.Config.Mounts)
	}

	// Send a message — spawnSession should forward mounts to the spawner.
	_, err := chatMgr.SendMessage("mounts-chat", "hello")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	if len(sp.lastReq.Mounts) == 0 {
		t.Fatal("expected mounts to be forwarded to SpawnSession, got none")
	}
	found := false
	for _, m := range sp.lastReq.Mounts {
		if m.Host == "my-workspace-vol" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected my-workspace-vol in spawned session mounts, got %+v", sp.lastReq.Mounts)
	}
}

// TestCreateChat_ClaudeConfigPropagated verifies that Claude config (including
// claude_md) is stored in the chat record and forwarded to SpawnSession.
func TestCreateChat_ClaudeConfigPropagated(t *testing.T) {
	ts, _, chatMgr, sp := newChatTestServer(t)

	claudeMD := "You are a helpful assistant. Use /workspace/persist for memory."
	resp := postJSON(t, ts, "/chats", apischema.CreateChatRequest{
		Name: "claude-config-chat",
		Config: apischema.ChatAPIConfig{
			Agent: "echo-test",
			Claude: &apischema.ClaudeConfig{
				ClaudeMD: claudeMD,
				MaxTurns: 5,
			},
		},
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(body))
	}

	// Verify Claude config is stored.
	var chatResp apischema.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chatResp.Config.Claude == nil || chatResp.Config.Claude.ClaudeMD != claudeMD {
		t.Fatalf("expected claude_md in response, got %+v", chatResp.Config.Claude)
	}

	// Send message — Claude config should be forwarded to spawner.
	_, err := chatMgr.SendMessage("claude-config-chat", "hello")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	if sp.lastReq.Claude == nil {
		t.Fatal("expected Claude config to be forwarded to SpawnSession")
	}
	if sp.lastReq.Claude.ClaudeMD != claudeMD {
		t.Fatalf("expected claude_md %q, got %q", claudeMD, sp.lastReq.Claude.ClaudeMD)
	}
	if sp.lastReq.Claude.MaxTurns != 5 {
		t.Fatalf("expected max_turns 5, got %d", sp.lastReq.Claude.MaxTurns)
	}
}

func TestChatLifecycle_CreateSendGetDelete(t *testing.T) {
	ts, _, _, _ := newChatTestServer(t)

	// 1. Create.
	created := mustCreateChat(t, ts, "lifecycle", "echo-test")
	if created.State != "created" {
		t.Fatalf("expected state %q, got %q", "created", created.State)
	}

	// 2. Send message → spawns.
	resp := postJSON(t, ts, "/chats/lifecycle/messages", apischema.SendMessageRequest{
		Message: "hello",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", resp.StatusCode, string(body))
	}
	var sendResp apischema.SendMessageResponse
	json.Unmarshal(body, &sendResp)
	if !sendResp.Spawned {
		t.Fatal("expected spawned=true")
	}

	// 3. Get chat — should be running.
	resp2 := get(t, ts, "/chats/lifecycle")
	body2 := readBody(t, resp2)
	var chatResp apischema.ChatResponse
	json.Unmarshal(body2, &chatResp)
	if chatResp.State != "running" {
		t.Fatalf("expected state %q, got %q", "running", chatResp.State)
	}
	if chatResp.CurrentSession != sendResp.SessionID {
		t.Fatalf("expected current_session %q, got %q", sendResp.SessionID, chatResp.CurrentSession)
	}

	// 4. List chats — should have 1.
	resp3 := get(t, ts, "/chats")
	body3 := readBody(t, resp3)
	var summaries []apischema.ChatSummary
	json.Unmarshal(body3, &summaries)
	if len(summaries) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(summaries))
	}
	if summaries[0].SessionCount != 1 {
		t.Fatalf("expected 1 session in chain, got %d", summaries[0].SessionCount)
	}

	// 5. Delete.
	resp4 := httpDelete(t, ts, "/chats/lifecycle")
	readBody(t, resp4)
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp4.StatusCode)
	}
}
