package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/danieliser/agentruntime/pkg/session"
)

func TestAdversarialCreateSession_UnknownAgent(t *testing.T) {
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

func TestAdversarialCreateSession_UnknownRuntime(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts, "/sessions", SessionRequest{
		Agent:   "echo-test",
		Runtime: "nonexistent",
		Prompt:  "hello",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "runtime") {
		t.Fatalf("expected runtime error, got %q", string(body))
	}
}

func TestAdversarialCreateSession_LongPromptHandled(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{
		Agent:  "sleep-test",
		Prompt: strings.Repeat("x", 1<<20),
	})

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+created.SessionID, nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Fatalf("expected non-5xx delete status, got %d", resp.StatusCode)
	}
}

func TestAdversarialCreateSession_NonexistentMountHost(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts, "/sessions", SessionRequest{
		Agent:  "sleep-test",
		Prompt: "hello",
		Mounts: []Mount{{
			Host:      "/definitely/does/not/exist/agentruntime",
			Container: "/workspace",
			Mode:      "rw",
		}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	listResp := get(t, ts, "/sessions")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected list status 200, got %d", listResp.StatusCode)
	}
	var summaries []SessionSummary
	decodeJSON(t, listResp.Body, &summaries)
	if len(summaries) != 0 {
		t.Fatalf("expected failed spawn to leave no sessions, got %d", len(summaries))
	}
}

func TestAdversarialGetLogs_NegativeCursor(t *testing.T) {
	ts, srv := newTestServer(t)

	sess := session.NewSession("task-negative-cursor", "echo-test", "test")
	if err := srv.sessions.Add(sess); err != nil {
		t.Fatalf("add session: %v", err)
	}
	payload := []byte("negative cursor replay")
	_, nextOffset := sess.Replay.WriteOffset(payload)

	resp := get(t, ts, "/sessions/"+sess.ID+"/logs?cursor=-1")
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
}

func TestAdversarialGetLogs_FutureCursor(t *testing.T) {
	ts, srv := newTestServer(t)

	sess := session.NewSession("task-future-cursor", "echo-test", "test")
	if err := srv.sessions.Add(sess); err != nil {
		t.Fatalf("add session: %v", err)
	}
	_, nextOffset := sess.Replay.WriteOffset([]byte("future cursor replay"))

	resp := get(t, ts, "/sessions/"+sess.ID+"/logs?cursor=99999999999")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("expected empty body, got %q", string(body))
	}
	if got := resp.Header.Get("Agentruntime-Log-Cursor"); got != strconv.FormatInt(nextOffset, 10) {
		t.Fatalf("expected cursor %d, got %q", nextOffset, got)
	}
}

func TestAdversarialGetLogs_NonNumericCursor(t *testing.T) {
	ts, srv := newTestServer(t)

	sess := session.NewSession("task-bad-cursor", "echo-test", "test")
	if err := srv.sessions.Add(sess); err != nil {
		t.Fatalf("add session: %v", err)
	}

	resp := get(t, ts, "/sessions/"+sess.ID+"/logs?cursor=abc")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestAdversarialListSessions_ConcurrentRequests(t *testing.T) {
	ts, srv := newTestServer(t)

	for i := 0; i < 10; i++ {
		sess := session.NewSession("task-concurrent", "echo-test", "test")
		if err := srv.sessions.Add(sess); err != nil {
			t.Fatalf("add session %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan string, 100)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := client.Get(ts.URL + "/sessions")
			if err != nil {
				errCh <- err.Error()
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errCh <- "unexpected status: " + resp.Status
				return
			}

			var summaries []SessionSummary
			if err := json.NewDecoder(resp.Body).Decode(&summaries); err != nil {
				errCh <- err.Error()
				return
			}
			if len(summaries) != 10 {
				errCh <- "unexpected session count: " + strconv.Itoa(len(summaries))
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent list failed: %s", err)
	}
}

func TestAdversarialDeleteSession_TwiceRapidly(t *testing.T) {
	ts, _ := newTestServer(t)

	created := mustCreateSession(t, ts, SessionRequest{
		Agent:  "sleep-test",
		Prompt: "ignored",
	})

	var wg sync.WaitGroup
	statusCh := make(chan int, 2)
	errCh := make(chan string, 2)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req, err := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+created.SessionID, nil)
			if err != nil {
				errCh <- err.Error()
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				errCh <- err.Error()
				return
			}
			defer resp.Body.Close()
			statusCh <- resp.StatusCode
		}()
	}

	wg.Wait()
	close(statusCh)
	close(errCh)

	for err := range errCh {
		t.Fatalf("delete request failed: %s", err)
	}

	var sawSuccess bool
	for status := range statusCh {
		if status >= http.StatusInternalServerError {
			t.Fatalf("expected non-5xx delete status, got %d", status)
		}
		if status == http.StatusOK {
			sawSuccess = true
		}
	}
	if !sawSuccess {
		t.Fatal("expected at least one delete request to succeed")
	}
}

func TestAdversarialCreateSession_EmptyJSONObject(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts, "/sessions", map[string]any{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestAdversarialCreateSession_UnknownFieldsIgnored(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Post(
		ts.URL+"/sessions",
		"application/json",
		strings.NewReader(`{"agent":"echo-test","prompt":"hello","extra":"value","nested":{"ignored":true}}`),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestAdversarialSessionWS_CompletedSessionHasError(t *testing.T) {
	ts, srv := newTestServer(t)

	sess := session.NewSession("task-completed", "echo-test", "test")
	sess.SetCompleted(0)
	if err := srv.sessions.Add(sess); err != nil {
		t.Fatalf("add session: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/sessions/" + sess.ID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected websocket dial to fail")
	}
	if resp == nil {
		t.Fatal("expected HTTP response for websocket failure")
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestAdversarialGetLogs_NonexistentSession(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts, "/sessions/does-not-exist/logs")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
