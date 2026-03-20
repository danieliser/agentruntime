package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/danieliser/agentruntime/pkg/session/agentsessions"
)

// --- test doubles ---

// fakeRuntime implements runtime.Runtime using real local processes
// so our API tests exercise actual subprocess lifecycle, not stubs.
// We use "echo", "cat", and "sleep" — universally available on POSIX.
type fakeRuntime struct {
	rt             *runtime.LocalRuntime
	sessionDirRoot string
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	return &fakeRuntime{
		rt:             runtime.NewLocalRuntime(),
		sessionDirRoot: t.TempDir(),
	}
}

func (f *fakeRuntime) Name() string { return "test" }

func (f *fakeRuntime) Spawn(ctx context.Context, cfg runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	if cfg.SessionDir != nil {
		sessionDir := filepath.Join(f.sessionDirRoot, cfg.SessionID)
		if err := os.MkdirAll(sessionDir, 0o755); err != nil {
			return nil, err
		}
		*cfg.SessionDir = sessionDir
	}
	return f.rt.Spawn(ctx, cfg)
}

func (f *fakeRuntime) Recover(_ context.Context) ([]runtime.ProcessHandle, error) {
	return nil, nil
}

func (f *fakeRuntime) Cleanup(_ context.Context) error { return nil }

// echoAgent wraps ClaudeAgent but overrides BuildCmd to use "/bin/echo" so tests
// run without Claude CLI installed. Verifies the agent/runtime pipeline end-to-end.
// Uses /bin/echo (not the shell builtin) so exec.Command can find it directly.
type echoAgent struct{}

func (a *echoAgent) Name() string { return "echo-test" }

func (a *echoAgent) BuildCmd(prompt string, _ agent.AgentConfig) ([]string, error) {
	// Use sh -c so we can interpose a sleep, giving the test WS client time to
	// connect before the process exits. Without the sleep, echo exits in microseconds
	// and the bridge completes before any WS client can dial.
	// The prompt is passed via arg to avoid shell injection.
	return []string{"/bin/sh", "-c", "/bin/echo \"$1\" && sleep 2", "--", prompt}, nil
}

func (a *echoAgent) ParseOutput(output []byte) (*agent.AgentResult, bool) { return nil, false }

// catAgent spawns "cat" which reads from stdin — lets us test interactive steering.
type catAgent struct{}

func (a *catAgent) Name() string { return "cat-test" }

func (a *catAgent) BuildCmd(prompt string, _ agent.AgentConfig) ([]string, error) {
	return []string{"cat"}, nil
}

func (a *catAgent) ParseOutput(output []byte) (*agent.AgentResult, bool) { return nil, false }

// sleepAgent spawns "sleep 60" for kill tests.
type sleepAgent struct{}

func (a *sleepAgent) Name() string { return "sleep-test" }

func (a *sleepAgent) BuildCmd(prompt string, _ agent.AgentConfig) ([]string, error) {
	return []string{"sleep", "60"}, nil
}

func (a *sleepAgent) ParseOutput(output []byte) (*agent.AgentResult, bool) { return nil, false }

type captureAgent struct {
	name string
	mu   sync.Mutex
	cfg  agent.AgentConfig
}

func (a *captureAgent) Name() string { return a.name }

func (a *captureAgent) BuildCmd(prompt string, cfg agent.AgentConfig) ([]string, error) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return []string{"/bin/echo", prompt}, nil
}

func (a *captureAgent) ParseOutput(output []byte) (*agent.AgentResult, bool) { return nil, false }

func (a *captureAgent) LastConfig() agent.AgentConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// --- test server builder ---

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	rt := newFakeRuntime(t)
	reg := agent.NewRegistry()
	reg.Register(&echoAgent{})
	reg.Register(&catAgent{})
	reg.Register(&sleepAgent{})
	mgr := session.NewManager()
	dataDir := t.TempDir()
	srv := NewServer(mgr, rt, reg, ServerConfig{
		DataDir: dataDir,
		LogDir:  filepath.Join(dataDir, "logs"),
	})
	ts := httptest.NewServer(srv.router)
	t.Cleanup(ts.Close)
	return ts, srv
}

func newConfiguredTestServer(t *testing.T, reg *agent.Registry, cfg ServerConfig) (*httptest.Server, *Server) {
	t.Helper()
	rt := newFakeRuntime(t)
	mgr := session.NewManager()
	srv := NewServer(mgr, rt, reg, cfg)
	ts := httptest.NewServer(srv.router)
	t.Cleanup(ts.Close)
	return ts, srv
}

func post(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
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

func get(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func decodeJSON(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustCreateSession(t *testing.T, ts *httptest.Server, req SessionRequest) SessionResponse {
	t.Helper()
	resp := post(t, ts, "/sessions", req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create session: expected 201, got %d body=%s", resp.StatusCode, string(body))
	}
	var created SessionResponse
	decodeJSON(t, resp.Body, &created)
	return created
}

func mustDeleteSession(t *testing.T, ts *httptest.Server, id string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete session %s: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete session %s: expected 200/404, got %d body=%s", id, resp.StatusCode, string(body))
	}
}

func mustDialSessionWS(t *testing.T, ts *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/sessions/" + id
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return conn
}

func mustReadConnectedFrame(t *testing.T, conn *websocket.Conn) bridge.ServerFrame {
	t.Helper()
	var frame bridge.ServerFrame
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("read connected frame: %v", err)
	}
	if frame.Type != "connected" {
		t.Fatalf("expected connected frame, got %q", frame.Type)
	}
	return frame
}

func mustWriteClientFrame(t *testing.T, conn *websocket.Conn, frame bridge.ClientFrame) {
	t.Helper()
	if err := conn.WriteJSON(frame); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
}

func waitForStdoutContaining(t *testing.T, conn *websocket.Conn, needle string) bridge.ServerFrame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var frame bridge.ServerFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read frame while waiting for %q: %v", needle, err)
		}
		if frame.Type == "stdout" && strings.Contains(frame.Data, needle) {
			return frame
		}
		if frame.Type == "error" {
			t.Fatalf("unexpected error frame while waiting for %q: %s", needle, frame.Error)
		}
		if frame.Type == "exit" {
			t.Fatalf("session exited before stdout containing %q arrived", needle)
		}
	}
	t.Fatalf("timed out waiting for stdout containing %q", needle)
	return bridge.ServerFrame{}
}

// --- health ---

func TestHealth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts, "/health")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", body["status"])
	}
	if body["default_runtime"] != "test" {
		t.Fatalf("expected default_runtime 'test', got %v", body["default_runtime"])
	}
}

// --- session creation ---

func TestCreateSession_Success(t *testing.T) {
	ts, _ := newTestServer(t)
	body := mustCreateSession(t, ts, SessionRequest{
		Agent:  "echo-test",
		Prompt: "hello",
	})

	if body.SessionID == "" {
		t.Fatal("expected non-empty session id")
	}
	if body.Status != string(session.StateRunning) {
		t.Fatalf("expected status 'running', got %v", body.Status)
	}
	if body.Agent != "echo-test" {
		t.Fatalf("expected agent 'echo-test', got %v", body.Agent)
	}
	if !strings.Contains(body.WSURL, "/ws/sessions/"+body.SessionID) {
		t.Fatalf("expected ws_url to include session path, got %q", body.WSURL)
	}
	if !strings.Contains(body.LogURL, "/sessions/"+body.SessionID+"/logs") {
		t.Fatalf("expected log_url to include logs path, got %q", body.LogURL)
	}
}

func TestCreateSession_ClaudeResumeSession(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir, err := agentsessions.InitClaudeSessionDir(dataDir, "resume-source", "/workspace", "")
	if err != nil {
		t.Fatalf("init claude session dir: %v", err)
	}
	sessionsDir := filepath.Join(sessionDir, "sessions")
	writeJSONFile(t, filepath.Join(sessionsDir, "5000.json"), map[string]any{
		"pid":       5000,
		"sessionId": "claude-native-123",
		"cwd":       "/workspace",
		"startedAt": 5000,
	})

	reg := agent.NewRegistry()
	capture := &captureAgent{name: "claude"}
	reg.Register(capture)
	ts, _ := newConfiguredTestServer(t, reg, ServerConfig{
		DataDir: dataDir,
		LogDir:  filepath.Join(dataDir, "logs"),
	})

	resp := post(t, ts, "/sessions", SessionRequest{
		Agent:         "claude",
		Prompt:        "resume this",
		ResumeSession: "resume-source",
		WorkDir:       t.TempDir(),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(body))
	}

	if got := capture.LastConfig().ResumeSessionID; got != "claude-native-123" {
		t.Fatalf("expected resolved claude session id, got %q", got)
	}
}

func TestCreateSession_CodexResumeSession(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir, err := agentsessions.InitCodexSessionDir(dataDir, "resume-source")
	if err != nil {
		t.Fatalf("init codex session dir: %v", err)
	}
	writeJSONFile(t, filepath.Join(sessionDir, "sessions", "latest.json"), map[string]any{
		"id": "codex-native-456",
	})

	reg := agent.NewRegistry()
	capture := &captureAgent{name: "codex"}
	reg.Register(capture)
	ts, _ := newConfiguredTestServer(t, reg, ServerConfig{
		DataDir: dataDir,
		LogDir:  filepath.Join(dataDir, "logs"),
	})

	resp := post(t, ts, "/sessions", SessionRequest{
		Agent:         "codex",
		Prompt:        "resume this",
		ResumeSession: "resume-source",
		WorkDir:       t.TempDir(),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(body))
	}

	if got := capture.LastConfig().ResumeSessionID; got != "codex-native-456" {
		t.Fatalf("expected resolved codex session id, got %q", got)
	}
}

func TestCreateSession_UnknownAgent(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts, "/sessions", SessionRequest{
		Agent:  "does-not-exist",
		Prompt: "hello",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateSession_MissingAgent(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts, "/sessions", map[string]string{
		"prompt": "hello",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing agent, got %d", resp.StatusCode)
	}
}

func TestCreateSession_MissingPrompt(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts, "/sessions", map[string]string{
		"agent": "echo-test",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing prompt, got %d", resp.StatusCode)
	}
}

func TestCreateSession_EmptyBody(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty body, got %d", resp.StatusCode)
	}
}

func TestCreateSession_InvalidJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader("{not json}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

// --- session get ---

func TestGetSession_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts, "/sessions/does-not-exist")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestGetSession_Found(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create a session first.
	created := mustCreateSession(t, ts, SessionRequest{Agent: "echo-test", Prompt: "hi"})
	id := created.SessionID

	// Get it back.
	resp2 := get(t, ts, "/sessions/"+id)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	var body map[string]any
	decodeJSON(t, resp2.Body, &body)
	if body["id"] != id {
		t.Fatalf("expected id %q, got %v", id, body["id"])
	}
}

func TestGetSessionInfo_ReturnsAllFields(t *testing.T) {
	ts, srv := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{
		TaskID: "task-info",
		Agent:  "sleep-test",
		Prompt: "ignored",
	})
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+created.SessionID, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	})

	resp := get(t, ts, "/sessions/"+created.SessionID+"/info")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var info SessionInfo
	decodeJSON(t, resp.Body, &info)

	sess := srv.sessions.Get(created.SessionID)
	if sess == nil {
		t.Fatalf("expected session %q in manager", created.SessionID)
	}
	snap := sess.Snapshot()
	expectedLogFile := session.LogFilePath(srv.logDir, created.SessionID)

	if info.SessionID != created.SessionID {
		t.Fatalf("expected session_id %q, got %q", created.SessionID, info.SessionID)
	}
	if info.TaskID != "task-info" {
		t.Fatalf("expected task_id %q, got %q", "task-info", info.TaskID)
	}
	if info.Agent != "sleep-test" {
		t.Fatalf("expected agent %q, got %q", "sleep-test", info.Agent)
	}
	if info.Runtime != "test" {
		t.Fatalf("expected runtime %q, got %q", "test", info.Runtime)
	}
	if info.Status != string(session.StateRunning) {
		t.Fatalf("expected status %q, got %q", session.StateRunning, info.Status)
	}
	if info.CreatedAt.IsZero() {
		t.Fatal("expected non-zero created_at")
	}
	if info.EndedAt != nil {
		t.Fatalf("expected nil ended_at for running session, got %v", info.EndedAt)
	}
	if info.ExitCode != nil {
		t.Fatalf("expected nil exit_code for running session, got %v", info.ExitCode)
	}
	if info.SessionDir == "" {
		t.Fatal("expected non-empty session_dir")
	}
	if info.SessionDir != snap.SessionDir {
		t.Fatalf("expected session_dir %q, got %q", snap.SessionDir, info.SessionDir)
	}
	if _, err := os.Stat(info.SessionDir); err != nil {
		t.Fatalf("expected session_dir to exist: %v", err)
	}
	if info.LogFile != expectedLogFile {
		t.Fatalf("expected log_file %q, got %q", expectedLogFile, info.LogFile)
	}
	if info.WSURL != created.WSURL {
		t.Fatalf("expected ws_url %q, got %q", created.WSURL, info.WSURL)
	}
	if info.LogURL != created.LogURL {
		t.Fatalf("expected log_url %q, got %q", created.LogURL, info.LogURL)
	}
}

func TestGetSessionInfo_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts, "/sessions/does-not-exist/info")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListSessions_Empty(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts, "/sessions")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var summaries []SessionSummary
	decodeJSON(t, resp.Body, &summaries)
	if len(summaries) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(summaries))
	}
}

func TestListSessions_WithSessions(t *testing.T) {
	ts, srv := newTestServer(t)

	sess1 := session.NewSession("task-1", "echo-test", "test", map[string]string{"suite": "api"})
	sess2 := session.NewSession("task-2", "cat-test", "test", map[string]string{"suite": "api", "kind": "interactive"})
	if err := srv.sessions.Add(sess1); err != nil {
		t.Fatalf("add session 1: %v", err)
	}
	if err := srv.sessions.Add(sess2); err != nil {
		t.Fatalf("add session 2: %v", err)
	}

	resp := get(t, ts, "/sessions")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var summaries []SessionSummary
	decodeJSON(t, resp.Body, &summaries)
	if len(summaries) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(summaries))
	}

	byID := make(map[string]SessionSummary, len(summaries))
	for _, summary := range summaries {
		byID[summary.SessionID] = summary
	}

	if got := byID[sess1.ID]; got.TaskID != "task-1" || got.Agent != "echo-test" || got.Runtime != "test" {
		t.Fatalf("unexpected session 1 summary: %+v", got)
	}
	if got := byID[sess2.ID]; got.TaskID != "task-2" || got.Tags["kind"] != "interactive" {
		t.Fatalf("unexpected session 2 summary: %+v", got)
	}
}

func TestGetLogs_ReturnsBufferedOutput(t *testing.T) {
	ts, srv := newTestServer(t)

	sess := session.NewSession("task-logs", "echo-test", "test")
	if err := srv.sessions.Add(sess); err != nil {
		t.Fatalf("add session: %v", err)
	}
	payload := []byte("hello\nworld\n")
	_, nextOffset := sess.Replay.WriteOffset(payload)

	resp := get(t, ts, "/sessions/"+sess.ID+"/logs")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(payload) {
		t.Fatalf("expected %q, got %q", string(payload), string(body))
	}
	if got := resp.Header.Get("Agentruntime-Log-Cursor"); got != strconv.FormatInt(nextOffset, 10) {
		t.Fatalf("expected cursor %d, got %q", nextOffset, got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("expected text/plain content type, got %q", ct)
	}
}

func TestGetLogs_CursorAdvances(t *testing.T) {
	ts, srv := newTestServer(t)

	sess := session.NewSession("task-logs", "echo-test", "test")
	if err := srv.sessions.Add(sess); err != nil {
		t.Fatalf("add session: %v", err)
	}
	payload := []byte("buffered output")
	_, nextOffset := sess.Replay.WriteOffset(payload)

	resp1 := get(t, ts, "/sessions/"+sess.ID+"/logs?cursor=0")
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first request expected 200, got %d", resp1.StatusCode)
	}
	body1, err := io.ReadAll(resp1.Body)
	if err != nil {
		t.Fatalf("read first body: %v", err)
	}
	if string(body1) != string(payload) {
		t.Fatalf("expected first body %q, got %q", string(payload), string(body1))
	}
	cursor := resp1.Header.Get("Agentruntime-Log-Cursor")
	if cursor != strconv.FormatInt(nextOffset, 10) {
		t.Fatalf("expected cursor %d, got %q", nextOffset, cursor)
	}

	resp2 := get(t, ts, "/sessions/"+sess.ID+"/logs?cursor="+cursor)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request expected 200, got %d", resp2.StatusCode)
	}
	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("read second body: %v", err)
	}
	if len(body2) != 0 {
		t.Fatalf("expected empty second body, got %q", string(body2))
	}
	if got := resp2.Header.Get("Agentruntime-Log-Cursor"); got != cursor {
		t.Fatalf("expected cursor to remain %q, got %q", cursor, got)
	}
}

func TestGetLogs_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts, "/sessions/does-not-exist/logs")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- session delete (kill) ---

func TestDeleteSession_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/no-such-session", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDeleteSession_KillsProcess(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create a session that would run forever.
	created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "ignored"})
	id := created.SessionID

	// Kill it.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+id, nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	// Session should be removed from manager after delete, or in a terminal state.
	// Poll briefly — the session may already be gone (removed on delete).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp3 := get(t, ts, "/sessions/"+id)
		var s map[string]any
		decodeJSON(t, resp3.Body, &s)
		resp3.Body.Close()
		// Session removed from manager — delete succeeded
		if s == nil || s["state"] == nil {
			return // success — session was removed
		}
		st, _ := s["state"].(string)
		if st == string(session.StateCompleted) || st == string(session.StateFailed) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("session did not reach a terminal state after kill")
}

// --- full lifecycle: create → WS → output arrives → process exits ---

// TestSessionLifecycle_EchoViaWS is the centrepiece integration test.
// It creates a session, connects via WebSocket, and asserts that:
//   - the "connected" frame arrives
//   - stdout output from the process arrives as a "stdout" frame
//   - the "exit" frame arrives with code 0
//
// This exercises the full stack: HTTP API → runtime.Spawn → bridge → WebSocket.
func TestSessionLifecycle_EchoViaWS(t *testing.T) {
	ts, _ := newTestServer(t)

	// 1. Create session.
	created := mustCreateSession(t, ts, SessionRequest{
		Agent:  "echo-test",
		Prompt: "hello from agent",
	})
	id := created.SessionID

	// 2. Connect WebSocket.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/sessions/" + id
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer conn.Close()

	// 3. Collect frames until exit.
	var frames []bridge.ServerFrame
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var f bridge.ServerFrame
		if err := conn.ReadJSON(&f); err != nil {
			t.Logf("ReadJSON error: %v (frames so far: %d)", err, len(frames))
			// Connection closed — normal after exit frame.
			break
		}
		t.Logf("frame received: type=%q data=%q exit_code=%v", f.Type, f.Data, f.ExitCode)
		frames = append(frames, f)
		if f.Type == "exit" {
			break
		}
	}

	// 4. Assert frame sequence.
	if len(frames) == 0 {
		t.Fatal("received no frames")
	}

	// First frame must be "connected".
	if frames[0].Type != "connected" {
		t.Fatalf("expected first frame 'connected', got %q", frames[0].Type)
	}
	if frames[0].SessionID != id {
		t.Fatalf("connected frame has wrong session_id: %q", frames[0].SessionID)
	}

	// At least one stdout frame with the echo output.
	var gotStdout bool
	for _, f := range frames {
		if f.Type == "stdout" && strings.Contains(f.Data, "hello from agent") {
			gotStdout = true
			break
		}
	}
	if !gotStdout {
		t.Fatalf("expected stdout frame containing 'hello from agent', got frames: %v", frames)
	}

	// Last meaningful frame must be exit with code 0.
	last := frames[len(frames)-1]
	if last.Type != "exit" {
		t.Fatalf("expected last frame 'exit', got %q", last.Type)
	}
	if last.ExitCode == nil || *last.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", last.ExitCode)
	}
}

// TestSessionLifecycle_StdinSteering verifies that data sent via WS stdin
// frame reaches the process stdin (cat echo-back pattern).
func TestSessionLifecycle_StdinSteering(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create cat session (reads stdin, echoes to stdout).
	created := mustCreateSession(t, ts, SessionRequest{
		Agent:       "cat-test",
		Interactive: true,
	})
	id := created.SessionID
	t.Cleanup(func() { mustDeleteSession(t, ts, id) })

	conn := mustDialSessionWS(t, ts, id)
	defer conn.Close()
	mustReadConnectedFrame(t, conn)

	mustWriteClientFrame(t, conn, bridge.ClientFrame{Type: "stdin", Data: "steered input\n"})
	waitForStdoutContaining(t, conn, "steered input")
}

// TestSessionLifecycle_WSNotFound verifies 404 before WS upgrade so the
// client receives an HTTP error instead of a silent hang.
func TestSessionLifecycle_WSNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/sessions/no-such-id"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected WS dial to fail for unknown session")
	}
	if resp != nil && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestSessionLifecycle_ReplayOnReconnect verifies that a reconnecting client
// can catch up on missed output via ?since=0.
//
// Scenario: process runs and exits. Client connects AFTER exit with since=0.
// Must receive replay frame containing the full output.
func TestSessionLifecycle_ReplayOnReconnect(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create and run a session to completion.
	created := mustCreateSession(t, ts, SessionRequest{
		Agent:  "echo-test",
		Prompt: "replay-this-output",
	})
	id := created.SessionID

	// First connection — drain until exit so replay buffer is populated.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/sessions/" + id
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("first WS dial: %v", err)
	}
	conn1.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var f bridge.ServerFrame
		if err := conn1.ReadJSON(&f); err != nil {
			break
		}
		if f.Type == "exit" {
			break
		}
	}
	conn1.Close()

	// Second connection with ?since=0 — must replay buffered output.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL+"?since=0", nil)
	if err != nil {
		t.Fatalf("reconnect WS dial: %v", err)
	}
	defer conn2.Close()
	conn2.SetReadDeadline(time.Now().Add(5 * time.Second))

	var gotReplay bool
	for {
		var f bridge.ServerFrame
		if err := conn2.ReadJSON(&f); err != nil {
			break
		}
		if f.Type == "replay" {
			// Replay data is base64-encoded; decode before checking content.
			decoded, err := base64.StdEncoding.DecodeString(f.Data)
			if err == nil && strings.Contains(string(decoded), "replay-this-output") {
				gotReplay = true
			}
			break
		}
		if f.Type == "connected" {
			// connected comes after replay — no replay frame was sent
			break
		}
	}
	if !gotReplay {
		t.Fatal("expected replay frame on reconnect, did not receive one")
	}
}

// TestSessionLifecycle_AppLevelPingPong verifies application-level ping/pong
// (distinct from WebSocket protocol-level ping/pong).
func TestSessionLifecycle_AppLevelPingPong(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "cat-test", Interactive: true})
	id := created.SessionID
	t.Cleanup(func() { mustDeleteSession(t, ts, id) })

	conn := mustDialSessionWS(t, ts, id)
	defer conn.Close()

	mustReadConnectedFrame(t, conn)

	// Send application-level ping.
	mustWriteClientFrame(t, conn, bridge.ClientFrame{Type: "ping"})

	// Expect pong.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f bridge.ServerFrame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if f.Type != "pong" {
		t.Fatalf("expected pong, got %q", f.Type)
	}
}

// TestSessionLifecycle_UnknownFrameType verifies the server sends an error
// frame for unrecognised client frame types instead of silently dropping them.
func TestSessionLifecycle_UnknownFrameType(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "cat-test", Interactive: true})
	id := created.SessionID
	t.Cleanup(func() { mustDeleteSession(t, ts, id) })

	conn := mustDialSessionWS(t, ts, id)
	defer conn.Close()

	mustReadConnectedFrame(t, conn)

	// Send garbage frame type.
	mustWriteClientFrame(t, conn, bridge.ClientFrame{Type: "definitely-not-a-real-type"})

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f bridge.ServerFrame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if f.Type != "error" {
		t.Fatalf("expected error frame, got %q", f.Type)
	}
	if f.Error == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestInteractiveSession_StdinRouting(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{
		Agent:       "cat-test",
		Interactive: true,
	})
	t.Cleanup(func() { mustDeleteSession(t, ts, created.SessionID) })

	conn := mustDialSessionWS(t, ts, created.SessionID)
	defer conn.Close()

	mustReadConnectedFrame(t, conn)
	mustWriteClientFrame(t, conn, bridge.ClientFrame{Type: "stdin", Data: "route this through stdin\n"})
	waitForStdoutContaining(t, conn, "route this through stdin")
}

func TestInteractiveSession_MultiplePrompts(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{
		Agent:       "cat-test",
		Interactive: true,
	})
	t.Cleanup(func() { mustDeleteSession(t, ts, created.SessionID) })

	conn := mustDialSessionWS(t, ts, created.SessionID)
	defer conn.Close()

	mustReadConnectedFrame(t, conn)

	mustWriteClientFrame(t, conn, bridge.ClientFrame{Type: "stdin", Data: "first prompt\n"})
	waitForStdoutContaining(t, conn, "first prompt")

	mustWriteClientFrame(t, conn, bridge.ClientFrame{Type: "stdin", Data: "second prompt\n"})
	waitForStdoutContaining(t, conn, "second prompt")
}

// TestConcurrentSessions verifies that multiple sessions can run in parallel
// without interfering with each other's output streams.
func TestConcurrentSessions(t *testing.T) {
	ts, _ := newTestServer(t)

	type result struct {
		id     string
		prompt string
		output string
		err    error
	}
	results := make(chan result, 3)

	prompts := []string{"session-A-output", "session-B-output", "session-C-output"}
	sessionIDs := make(map[string]string, len(prompts))
	for _, prompt := range prompts {
		created := mustCreateSession(t, ts, SessionRequest{
			Agent:       "cat-test",
			Interactive: true,
		})
		sessionIDs[prompt] = created.SessionID
		t.Cleanup(func() { mustDeleteSession(t, ts, created.SessionID) })
	}

	for _, prompt := range prompts {
		prompt := prompt
		id := sessionIDs[prompt]
		go func() {
			wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/sessions/" + id
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
			if err != nil {
				results <- result{id: id, prompt: prompt, err: err}
				return
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))

			var connected bridge.ServerFrame
			if err := conn.ReadJSON(&connected); err != nil {
				results <- result{id: id, prompt: prompt, err: err}
				return
			}
			if connected.Type != "connected" {
				results <- result{id: id, prompt: prompt, err: io.ErrUnexpectedEOF}
				return
			}

			if err := conn.WriteJSON(bridge.ClientFrame{Type: "stdin", Data: prompt + "\n"}); err != nil {
				results <- result{id: id, prompt: prompt, err: err}
				return
			}

			var combined strings.Builder
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				var f bridge.ServerFrame
				if err := conn.ReadJSON(&f); err != nil {
					if websocket.IsUnexpectedCloseError(err) {
						results <- result{id: id, prompt: prompt, err: err}
						return
					}
					if strings.Contains(err.Error(), "i/o timeout") {
						continue
					}
					results <- result{id: id, prompt: prompt, err: err}
					return
				}
				if f.Type == "stdout" {
					combined.WriteString(f.Data)
					if strings.Contains(combined.String(), prompt) {
						results <- result{id: id, prompt: prompt, output: combined.String()}
						return
					}
				}
				if f.Type == "error" {
					results <- result{id: id, prompt: prompt, err: io.ErrUnexpectedEOF}
					return
				}
			}

			results <- result{
				id:     id,
				prompt: prompt,
				output: combined.String(),
				err:    context.DeadlineExceeded,
			}
		}()
	}

	// Collect and verify no cross-contamination.
	seen := make(map[string]bool)
	for i := 0; i < len(prompts); i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("session %s (%s) failed: %v", r.id, r.prompt, r.err)
		}
		for _, p := range prompts {
			if strings.Contains(r.output, p) {
				if seen[p] {
					t.Errorf("output for prompt %q appeared in multiple sessions", p)
				}
				seen[p] = true
			}
		}
	}
	for _, p := range prompts {
		if !seen[p] {
			t.Errorf("prompt %q never appeared in any session output", p)
		}
	}
}

// TestSessionState_TransitionsToCompleted verifies that after a short-lived
// process exits, GET /sessions/:id reflects Completed or Failed (not Running).
func TestSessionState_TransitionsToCompleted(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "echo-test", Prompt: "bye"})
	id := created.SessionID

	// Wait for process to exit (echo exits immediately).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp2 := get(t, ts, "/sessions/"+id)
		var s map[string]any
		decodeJSON(t, resp2.Body, &s)
		resp2.Body.Close()
		st := s["state"].(string)
		if st == string(session.StateCompleted) || st == string(session.StateFailed) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("session never transitioned out of Running state")
}

// TestLookupResumeSessionID_PrefersTagOverFilesystem verifies that when
// a completed session has claude_session_id in its tags, the resume lookup
// uses that value instead of scanning the filesystem. This fixes Docker
// resume where named volumes shadow host-side session directories (#5).
func TestLookupResumeSessionID_PrefersTagOverFilesystem(t *testing.T) {
	_, srv := newTestServer(t)

	// Create a completed session with claude_session_id tag.
	sess := session.NewSessionWithID("orig-sess", "", "claude", "test")
	sess.SetRunning(nil)
	sess.SetTag("claude_session_id", "claude-uuid-from-tag")
	sess.SetCompleted(0)
	srv.sessions.Add(sess)

	// Also set up a filesystem session with a different ID to prove tag wins.
	agentsessions.InitClaudeSessionDir(srv.dataDir, "orig-sess", "/workspace", "")
	sessionDir := filepath.Join(srv.dataDir, "claude-sessions", "orig-sess", "sessions")
	writeJSONFile(t, filepath.Join(sessionDir, "1000.json"), map[string]any{
		"pid": 1000, "sessionId": "claude-uuid-from-filesystem", "cwd": "/workspace", "startedAt": 100,
	})

	// lookupResumeSessionID should prefer the tag.
	got, err := srv.lookupResumeSessionID("claude", "orig-sess", sess)
	if err != nil {
		t.Fatalf("lookupResumeSessionID: %v", err)
	}
	if got != "claude-uuid-from-tag" {
		t.Fatalf("expected claude-uuid-from-tag, got %q (should prefer tag over filesystem)", got)
	}
}

// TestLookupResumeSessionID_FallsBackToFilesystem verifies that when
// no session tag exists, the lookup falls back to filesystem scanning.
func TestLookupResumeSessionID_FallsBackToFilesystem(t *testing.T) {
	_, srv := newTestServer(t)

	// Create a completed session WITHOUT claude_session_id tag.
	sess := session.NewSessionWithID("no-tag-sess", "", "claude", "test")
	sess.SetRunning(nil)
	sess.SetCompleted(0)
	srv.sessions.Add(sess)

	// Set up filesystem session.
	agentsessions.InitClaudeSessionDir(srv.dataDir, "no-tag-sess", "/workspace", "")
	sessionDir := filepath.Join(srv.dataDir, "claude-sessions", "no-tag-sess", "sessions")
	writeJSONFile(t, filepath.Join(sessionDir, "1000.json"), map[string]any{
		"pid": 1000, "sessionId": "claude-uuid-from-filesystem", "cwd": "/workspace", "startedAt": 100,
	})

	got, err := srv.lookupResumeSessionID("claude", "no-tag-sess", sess)
	if err != nil {
		t.Fatalf("lookupResumeSessionID: %v", err)
	}
	if got != "claude-uuid-from-filesystem" {
		t.Fatalf("expected claude-uuid-from-filesystem, got %q", got)
	}
}

// TestLookupResumeSessionID_NilSession verifies graceful handling when
// the original session is nil (not found in manager).
func TestLookupResumeSessionID_NilSession(t *testing.T) {
	_, srv := newTestServer(t)

	// Set up filesystem session.
	agentsessions.InitClaudeSessionDir(srv.dataDir, "gone-sess", "/workspace", "")
	sessionDir := filepath.Join(srv.dataDir, "claude-sessions", "gone-sess", "sessions")
	writeJSONFile(t, filepath.Join(sessionDir, "1000.json"), map[string]any{
		"pid": 1000, "sessionId": "claude-uuid-from-filesystem", "cwd": "/workspace", "startedAt": 100,
	})

	// nil original session — should fall back to filesystem.
	got, err := srv.lookupResumeSessionID("claude", "gone-sess", nil)
	if err != nil {
		t.Fatalf("lookupResumeSessionID: %v", err)
	}
	if got != "claude-uuid-from-filesystem" {
		t.Fatalf("expected claude-uuid-from-filesystem, got %q", got)
	}
}

// TestLookupResumeSessionID_Passthrough verifies that a pre-resolved Claude
// session ID is passed through when no agentruntime session or filesystem
// match exists. This is the path used by the chat manager which stores
// Claude session IDs in the ChatRecord and passes them directly.
func TestLookupResumeSessionID_Passthrough(t *testing.T) {
	_, srv := newTestServer(t)

	// No session in registry, no filesystem data — the ID should pass through.
	claudeID := "4e8f8c99-9191-4971-a972-cee11464ab41"
	got, err := srv.lookupResumeSessionID("claude", claudeID, nil)
	if err != nil {
		t.Fatalf("lookupResumeSessionID: %v", err)
	}
	if got != claudeID {
		t.Fatalf("expected passthrough %q, got %q", claudeID, got)
	}
}
