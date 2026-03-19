package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

// captureRun redirects os.Stdout and os.Stderr, runs fn(), and returns its
// return code along with the captured output.
func captureRun(fn func() int) (code int, stdout, stderr string) {
	oldOut := os.Stdout
	oldErr := os.Stderr
	defer func() {
		os.Stdout = oldOut
		os.Stderr = oldErr
	}()

	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	code = fn()

	wOut.Close()
	wErr.Close()

	var bufOut, bufErr bytes.Buffer
	io.Copy(&bufOut, rOut) //nolint:errcheck
	io.Copy(&bufErr, rErr) //nolint:errcheck
	return code, bufOut.String(), bufErr.String()
}

// testPort extracts the port number string from an httptest server URL.
func testPort(ts *httptest.Server) string {
	parts := strings.Split(ts.URL, ":")
	return parts[len(parts)-1]
}

// --- chat create ---

func TestChatCreate_Success(t *testing.T) {
	var gotReq apischema.CreateChatRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chats" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotReq) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(apischema.ChatResponse{ //nolint:errcheck
			Name:  "web-ui",
			State: "created",
		})
	}))
	defer ts.Close()

	code, stdout, _ := captureRun(func() int {
		return runChatCreate([]string{"--agent=claude", "--port", testPort(ts), "web-ui"})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if gotReq.Name != "web-ui" {
		t.Fatalf("expected name=web-ui, got %q", gotReq.Name)
	}
	if gotReq.Config.Agent != "claude" {
		t.Fatalf("expected agent=claude, got %q", gotReq.Config.Agent)
	}
	if !strings.Contains(stdout, `Created chat "web-ui"`) {
		t.Fatalf("expected success message in stdout, got %q", stdout)
	}
}

func TestChatCreate_Duplicate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "chat already exists"}) //nolint:errcheck
	}))
	defer ts.Close()

	code, _, stderr := captureRun(func() int {
		return runChatCreate([]string{"--agent=claude", "--port", testPort(ts), "web-ui"})
	})

	if code == 0 {
		t.Fatal("expected non-zero exit for duplicate chat")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("expected 'already exists' in stderr, got %q", stderr)
	}
}

func TestChatCreate_FromYAML(t *testing.T) {
	var gotReq apischema.CreateChatRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotReq) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(apischema.ChatResponse{Name: "code-review", State: "created"}) //nolint:errcheck
	}))
	defer ts.Close()

	// Write a temporary YAML config file.
	cfgFile := t.TempDir() + "/chat.yaml"
	yamlContent := "agent: claude\nruntime: docker\nmodel: claude-opus-4-6\nidle_timeout: 15m\n"
	if err := os.WriteFile(cfgFile, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	code, _, _ := captureRun(func() int {
		return runChatCreate([]string{"--config", cfgFile, "--port", testPort(ts), "code-review"})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if gotReq.Config.Agent != "claude" {
		t.Fatalf("expected agent=claude from YAML, got %q", gotReq.Config.Agent)
	}
	if gotReq.Config.Runtime != "docker" {
		t.Fatalf("expected runtime=docker from YAML, got %q", gotReq.Config.Runtime)
	}
	if gotReq.Config.IdleTimeout != "15m" {
		t.Fatalf("expected idle_timeout=15m from YAML, got %q", gotReq.Config.IdleTimeout)
	}
}

// --- chat send ---

func TestChatSend_Success(t *testing.T) {
	var gotReq apischema.SendMessageRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chats/web-ui/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotReq) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(apischema.SendMessageResponse{ //nolint:errcheck
			SessionID: "abc12345-0000-0000-0000-000000000000",
			State:     "running",
			WSURL:     "ws://localhost/ws/chats/web-ui",
		})
	}))
	defer ts.Close()

	code, _, stderr := captureRun(func() int {
		return runChatSend([]string{"--port", testPort(ts), "web-ui", "hello world"})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if gotReq.Message != "hello world" {
		t.Fatalf("expected message='hello world', got %q", gotReq.Message)
	}
	if !strings.Contains(stderr, "abc12345") {
		t.Fatalf("expected session ID in stderr, got %q", stderr)
	}
}

func TestChatSend_Busy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"error":          "chat is busy",
			"retry_after_ms": 5000,
		})
	}))
	defer ts.Close()

	code, _, stderr := captureRun(func() int {
		return runChatSend([]string{"--port", testPort(ts), "web-ui", "hello"})
	})

	if code == 0 {
		t.Fatal("expected non-zero exit for busy chat")
	}
	if !strings.Contains(stderr, "busy") {
		t.Fatalf("expected 'busy' in stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "5000") {
		t.Fatalf("expected retry_after_ms in stderr, got %q", stderr)
	}
}

// --- chat list ---

func TestChatList_Table(t *testing.T) {
	lastActive := time.Date(2026, 3, 19, 14, 32, 0, 0, time.UTC)
	summaries := []apischema.ChatSummary{
		{Name: "web-ui", State: "running", Agent: "claude", SessionCount: 3, LastActiveAt: &lastActive},
		{Name: "code-review", State: "idle", Agent: "claude", SessionCount: 1},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chats" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summaries) //nolint:errcheck
	}))
	defer ts.Close()

	code, stdout, _ := captureRun(func() int {
		return runChatList([]string{"--port", testPort(ts)})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	for _, col := range []string{"NAME", "STATE", "AGENT", "SESSIONS", "LAST ACTIVE"} {
		if !strings.Contains(stdout, col) {
			t.Fatalf("expected column %q in output, got:\n%s", col, stdout)
		}
	}
	if !strings.Contains(stdout, "web-ui") || !strings.Contains(stdout, "code-review") {
		t.Fatalf("expected chat names in output, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "2026-03-19") {
		t.Fatalf("expected last active date in output, got:\n%s", stdout)
	}
}

func TestChatList_JSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"web-ui","state":"running","agent":"claude","session_count":1}]`)) //nolint:errcheck
	}))
	defer ts.Close()

	code, stdout, _ := captureRun(func() int {
		return runChatList([]string{"--json", "--port", testPort(ts)})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout, `"web-ui"`) {
		t.Fatalf("expected raw JSON in stdout, got %q", stdout)
	}
}

func TestChatList_Empty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`)) //nolint:errcheck
	}))
	defer ts.Close()

	code, stdout, _ := captureRun(func() int {
		return runChatList([]string{"--port", testPort(ts)})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "No chats.") {
		t.Fatalf("expected 'No chats.' in stdout, got %q", stdout)
	}
}

// --- chat delete ---

func TestChatDelete_Default(t *testing.T) {
	var gotURL string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	code, stdout, _ := captureRun(func() int {
		return runChatDelete([]string{"--port", testPort(ts), "web-ui"})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if strings.Contains(gotURL, "remove_volume") {
		t.Fatalf("expected no remove_volume param, got URL %q", gotURL)
	}
	if !strings.Contains(stdout, `Deleted chat "web-ui"`) {
		t.Fatalf("expected success message, got %q", stdout)
	}
}

func TestChatDelete_RemoveVolume(t *testing.T) {
	var gotURL string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	code, _, _ := captureRun(func() int {
		return runChatDelete([]string{"--remove-volume", "--port", testPort(ts), "web-ui"})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(gotURL, "remove_volume=true") {
		t.Fatalf("expected remove_volume=true in URL, got %q", gotURL)
	}
}

// --- attach by chat name ---

func TestAttach_ByName(t *testing.T) {
	const chatName = "web-ui"
	const sessionID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	var resolveCount int
	wsUpgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chats/" + chatName:
			resolveCount++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(apischema.ChatResponse{ //nolint:errcheck
				Name:           chatName,
				State:          "running",
				CurrentSession: sessionID,
			})

		case "/ws/sessions/" + sessionID:
			conn, err := wsUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.WriteJSON(ServerFrame{Type: "connected", SessionID: sessionID}) //nolint:errcheck
			code := 0
			conn.WriteJSON(ServerFrame{Type: "exit", ExitCode: &code}) //nolint:errcheck

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	port := testPort(ts)
	done := make(chan int, 1)
	go func() {
		code, _, _ := captureRun(func() int {
			return runAttachCommand([]string{"--port", port, "--no-replay", chatName})
		})
		done <- code
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach by name timed out")
	}

	if resolveCount != 1 {
		t.Fatalf("expected 1 GET /chats/%s, got %d", chatName, resolveCount)
	}
}

func TestAttach_ByNameNotRunning(t *testing.T) {
	const chatName = "web-ui"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chats/"+chatName {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(apischema.ChatResponse{ //nolint:errcheck
				Name:  chatName,
				State: "idle",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	code, _, stderr := captureRun(func() int {
		return runAttachCommand([]string{"--port", testPort(ts), chatName})
	})

	if code == 0 {
		t.Fatal("expected non-zero exit for non-running chat")
	}
	if !strings.Contains(stderr, "not running") {
		t.Fatalf("expected 'not running' in stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "idle") {
		t.Fatalf("expected state 'idle' in stderr, got %q", stderr)
	}
}
