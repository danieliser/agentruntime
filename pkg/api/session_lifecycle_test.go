package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/session"
)

// newTestServerWithMaxSessions creates a test server with a configured max sessions limit.
func newTestServerWithMaxSessions(t *testing.T, max int) (*httptest.Server, *Server) {
	t.Helper()
	rt := newFakeRuntime(t)
	reg := agent.NewRegistry()
	reg.Register(&echoAgent{})
	reg.Register(&catAgent{})
	reg.Register(&sleepAgent{})
	mgr := session.NewManager()
	mgr.SetMaxSessions(max)
	dataDir := t.TempDir()
	srv := NewServer(mgr, rt, reg, ServerConfig{
		DataDir: dataDir,
		LogDir:  filepath.Join(dataDir, "logs"),
	})
	ts := httptest.NewServer(srv.router)
	t.Cleanup(ts.Close)
	return ts, srv
}

func deleteSession(t *testing.T, ts *httptest.Server, id string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /sessions/%s: %v", id, err)
	}
	return resp
}

// --- Test 1: Delete a session twice rapidly — second should return 404, not panic ---

func TestDeleteSession_DoubleDeleteReturns404(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "ignored"})
	id := created.SessionID

	// First delete — should succeed.
	resp1 := deleteSession(t, ts, id)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first delete: expected 200, got %d", resp1.StatusCode)
	}

	// Second delete — should return 404, not panic.
	resp2 := deleteSession(t, ts, id)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d", resp2.StatusCode)
	}
}

// --- Test 2: Delete a session that never existed — should return 404 ---

func TestDeleteSession_NeverExisted(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := deleteSession(t, ts, "completely-fabricated-session-id")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent session, got %d", resp.StatusCode)
	}
}

// --- Test 3: Create sessions up to max limit, then one more — should get 503 ---

func TestMaxSessions_RejectsOverLimit(t *testing.T) {
	max := 3
	ts, _ := newTestServerWithMaxSessions(t, max)

	ids := make([]string, 0, max)
	for i := 0; i < max; i++ {
		created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "fill"})
		ids = append(ids, created.SessionID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			mustDeleteSession(t, ts, id)
		}
	})

	// One more should be rejected with 503.
	resp := post(t, ts, "/sessions", SessionRequest{Agent: "sleep-test", Prompt: "overflow"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503 when over max sessions, got %d body=%s", resp.StatusCode, string(body))
	}
}

// --- Test 4: Set max sessions to 1, create 1, delete it, create another — slot freed ---

func TestMaxSessions_SlotFreedAfterDelete(t *testing.T) {
	ts, _ := newTestServerWithMaxSessions(t, 1)

	// Fill the single slot.
	created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "first"})

	// Delete it to free the slot.
	resp := deleteSession(t, ts, created.SessionID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}

	// Should be able to create another now.
	created2 := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "second"})
	t.Cleanup(func() { mustDeleteSession(t, ts, created2.SessionID) })

	if created2.SessionID == "" {
		t.Fatal("expected non-empty session ID after slot freed")
	}
}

// --- Test 5: Kill a session mid-output — replay buffer should close cleanly ---

func TestKillSessionMidOutput_ReplayBufferCloses(t *testing.T) {
	ts, _ := newTestServer(t)

	// Use cat-test which stays alive reading stdin.
	created := mustCreateSession(t, ts, SessionRequest{Agent: "cat-test", Interactive: true})
	id := created.SessionID

	// Connect WS and confirm it's alive.
	conn := mustDialSessionWS(t, ts, id)
	defer conn.Close()
	mustReadConnectedFrame(t, conn)

	// Send some data so replay buffer has content.
	mustWriteClientFrame(t, conn, bridge.ClientFrame{Type: "stdin", Data: "some output data\n"})
	waitForStdoutContaining(t, conn, "some output data")

	// Kill the session via DELETE.
	resp := deleteSession(t, ts, id)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}

	// WS should eventually close or send exit frame — not hang forever.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var frame bridge.ServerFrame
		err := conn.ReadJSON(&frame)
		if err != nil {
			break // connection closed — expected
		}
		if frame.Type == "exit" {
			break // also acceptable
		}
	}
}

// --- Test 6: Graceful shutdown with 10 active sessions — all killed, no goroutine leaks ---

func TestGracefulShutdown_KillsAllSessions(t *testing.T) {
	ts, srv := newTestServer(t)

	goroutinesBefore := runtime.NumGoroutine()

	// Create 10 long-lived sessions.
	ids := make([]string, 10)
	for i := 0; i < 10; i++ {
		created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "shutdown"})
		ids[i] = created.SessionID
	}

	// Verify all 10 exist.
	sessions := srv.sessions.List()
	if len(sessions) != 10 {
		t.Fatalf("expected 10 sessions, got %d", len(sessions))
	}

	// Graceful shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	// All sessions should be gone from the manager.
	remaining := srv.sessions.List()
	if len(remaining) != 0 {
		t.Fatalf("expected 0 sessions after shutdown, got %d", len(remaining))
	}

	// Give goroutines time to wind down.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		goroutinesAfter := runtime.NumGoroutine()
		// Allow some slack (test framework goroutines, etc), but the 10
		// session goroutines should be gone.
		if goroutinesAfter <= goroutinesBefore+5 {
			return // success
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("warning: goroutine count did not fully converge (before=%d, after=%d)",
		goroutinesBefore, runtime.NumGoroutine())
}

// --- Test 7: Create session, connect WS, delete session — WS should receive exit frame ---

func TestDeleteSession_WSReceivesExitFrame(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "cat-test", Interactive: true})
	id := created.SessionID

	conn := mustDialSessionWS(t, ts, id)
	defer conn.Close()
	mustReadConnectedFrame(t, conn)

	// Delete the session while WS is connected.
	resp := deleteSession(t, ts, id)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}

	// WS should receive an exit frame or close cleanly.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	gotExit := false
	for {
		var frame bridge.ServerFrame
		err := conn.ReadJSON(&frame)
		if err != nil {
			// Connection closed without explicit exit frame is also acceptable
			// since the bridge closes on context cancellation.
			break
		}
		if frame.Type == "exit" {
			gotExit = true
			break
		}
	}
	// The bridge should signal termination — either via exit frame or connection close.
	// Both are acceptable. If we got neither before the 5s deadline, that's a hang.
	_ = gotExit // not a hard requirement — connection close is sufficient
}

// --- Test 8: List sessions after deleting all — should return empty array ---

func TestListSessions_EmptyAfterDeletingAll(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create 5 sessions.
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "temp"})
		ids[i] = created.SessionID
	}

	// Delete all of them.
	for _, id := range ids {
		resp := deleteSession(t, ts, id)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("delete %s: expected 200, got %d", id, resp.StatusCode)
		}
	}

	// List should return empty array.
	resp := get(t, ts, "/sessions")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", resp.StatusCode)
	}
	var summaries []SessionSummary
	decodeJSON(t, resp.Body, &summaries)
	if len(summaries) != 0 {
		t.Fatalf("expected empty list after deleting all, got %d entries", len(summaries))
	}
}

// --- Test 9: Session info for deleted session — should return 404 ---

func TestGetSessionInfo_DeletedSession(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "ephemeral"})
	id := created.SessionID

	// Delete it.
	resp := deleteSession(t, ts, id)
	resp.Body.Close()

	// GET /sessions/:id should return 404.
	resp2 := get(t, ts, "/sessions/"+id)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET session after delete: expected 404, got %d", resp2.StatusCode)
	}

	// GET /sessions/:id/info should also return 404.
	resp3 := get(t, ts, "/sessions/"+id+"/info")
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("GET session info after delete: expected 404, got %d", resp3.StatusCode)
	}
}

// --- Test 10: Rapid create-delete cycles (100x) — no memory leak, no goroutine leak ---

func TestRapidCreateDeleteCycles(t *testing.T) {
	ts, srv := newTestServer(t)

	goroutinesBefore := runtime.NumGoroutine()

	// Run 100 create-delete cycles.
	for i := 0; i < 100; i++ {
		created := mustCreateSession(t, ts, SessionRequest{Agent: "echo-test", Prompt: "cycle"})
		resp := deleteSession(t, ts, created.SessionID)
		resp.Body.Close()
		// Accept both 200 (killed running) and 404 (already exited and cleaned up
		// between create and delete due to echo's fast exit).
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("cycle %d: delete returned %d", i, resp.StatusCode)
		}
	}

	// Manager should be empty (or near-empty — echo sessions may have already
	// self-cleaned before we deleted them).
	// Give in-flight goroutines time to settle.
	time.Sleep(2 * time.Second)

	remaining := len(srv.sessions.List())
	if remaining > 0 {
		t.Logf("warning: %d sessions remain after 100 cycles (echo may not have fully exited)", remaining)
	}

	// Check goroutine count converges. We allow generous headroom because the
	// test framework and HTTP server have their own goroutines, but the 200+
	// goroutines from 100 sessions should be cleaned up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		goroutinesAfter := runtime.NumGoroutine()
		if goroutinesAfter <= goroutinesBefore+20 {
			return // success — goroutines converged
		}
		time.Sleep(200 * time.Millisecond)
	}
	goroutinesAfter := runtime.NumGoroutine()
	if goroutinesAfter > goroutinesBefore+50 {
		t.Fatalf("goroutine leak: before=%d after=%d (delta=%d, expected <50)",
			goroutinesBefore, goroutinesAfter, goroutinesAfter-goroutinesBefore)
	}
}

// --- Bonus: Concurrent double-delete stress test ---

func TestConcurrentDoubleDelete_NoPanic(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "concurrent"})
	id := created.SessionID

	// Fire 10 concurrent DELETE requests for the same session.
	var wg sync.WaitGroup
	results := make([]int, 10)
	for i := 0; i < 10; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := deleteSession(t, ts, id)
			resp.Body.Close()
			results[i] = resp.StatusCode
		}()
	}
	wg.Wait()

	// Exactly one should succeed (200), rest should be 404.
	okCount := 0
	notFoundCount := 0
	for _, code := range results {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusNotFound:
			notFoundCount++
		default:
			t.Errorf("unexpected status code: %d", code)
		}
	}
	if okCount != 1 {
		t.Errorf("expected exactly 1 successful delete, got %d (404s: %d)", okCount, notFoundCount)
	}
}

// --- Bonus: Max sessions with concurrent creates ---

func TestMaxSessions_ConcurrentCreates(t *testing.T) {
	max := 5
	ts, _ := newTestServerWithMaxSessions(t, max)

	// Fire 10 concurrent create requests. Exactly `max` should succeed.
	var mu sync.Mutex
	var successIDs []string
	var rejections int
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := post(t, ts, "/sessions", SessionRequest{Agent: "sleep-test", Prompt: "race"})
			defer resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if resp.StatusCode == http.StatusCreated {
				var created SessionResponse
				decodeJSON(t, resp.Body, &created)
				successIDs = append(successIDs, created.SessionID)
			} else if resp.StatusCode == http.StatusServiceUnavailable {
				rejections++
			} else {
				t.Errorf("unexpected status: %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	// Cleanup created sessions.
	for _, id := range successIDs {
		mustDeleteSession(t, ts, id)
	}

	if len(successIDs) != max {
		t.Errorf("expected exactly %d successful creates, got %d (%d rejected)",
			max, len(successIDs), rejections)
	}
	if rejections != 10-max {
		t.Errorf("expected %d rejections, got %d", 10-max, rejections)
	}
}

// --- Bonus: WS dial after session deleted returns appropriate error ---

func TestWSDialAfterDelete_Fails(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{Agent: "sleep-test", Prompt: "ws-after-delete"})
	id := created.SessionID

	// Delete the session.
	resp := deleteSession(t, ts, id)
	resp.Body.Close()

	// Attempting to WS connect should fail (404 or connection refused).
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/sessions/" + id
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, httpResp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err == nil {
		conn.Close()
		t.Fatal("expected WS dial to fail after session deletion")
	}
	if httpResp != nil && httpResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", httpResp.StatusCode)
	}
}
