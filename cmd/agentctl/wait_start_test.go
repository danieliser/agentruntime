package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// fakeSession serves one session view, its approvals, its events and its
// event stream. Stream replays every event after the requested cursor.
type fakeSession struct {
	mu        sync.Mutex
	state     string
	approvals int
	events    []map[string]any
	wsAfter   []string
	eventsAt  []string
}

func (f *fakeSession) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeSession) handler() http.Handler {
	upgrader := websocket.Upgrader{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/ws/"):
			after := r.URL.Query().Get("after_sequence")
			f.wsAfter = append(f.wsAfter, after)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			var from int64
			fmt.Sscan(after, &from)
			for _, ev := range f.events {
				if ev["sequence"].(int64) > from {
					_ = conn.WriteJSON(ev)
				}
			}
		case strings.HasSuffix(r.URL.Path, "/approvals"):
			list := make([]map[string]any, f.approvals)
			for i := range list {
				list[i] = map[string]any{"request_id": i + 1}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"session_id": "s1", "approvals": list}})
		case strings.HasSuffix(r.URL.Path, "/events"):
			f.eventsAt = append(f.eventsAt, r.URL.Query().Get("after_sequence"))
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"events": []any{}, "last_sequence": len(f.events), "has_more": false}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"session_id": "s1", "state": f.state, "last_sequence": len(f.events)}})
		}
	})
}

func (f *fakeSession) add(kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, map[string]any{"sequence": int64(len(f.events) + 1), "type": kind, "stream": "provider_stdout"})
}

func waitContext(srv *httptest.Server, caller string, cursors *CursorStore) (*Context, *strings.Builder) {
	cfg := Config{URL: srv.URL, Caller: caller}
	out := &strings.Builder{}
	return &Context{Config: cfg, API: NewAPIClient(cfg), Out: out, Log: &strings.Builder{}, Cursors: cursors}, out
}

func TestWaitNewCallerIgnoresOldTurnEnd(t *testing.T) {
	f := &fakeSession{state: "running"}
	f.add("lifecycle.turn.started")
	f.add("turn.completed") // an old turn, already over
	f.add("lifecycle.turn.started")
	srv := f.server(t)
	dir := t.TempDir()
	ctx, _ := waitContext(srv, "fresh", nil)
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", dir, "--until", "turn_end", "--timeout", "300ms"})
	if ExitCode(err) != ExitTimeout {
		t.Fatalf("want timeout, got %v (code %d)", err, ExitCode(err))
	}
	if len(f.wsAfter) == 0 || f.wsAfter[0] != "3" {
		t.Fatalf("stream should start at the tail, got %v", f.wsAfter)
	}
}

func TestWaitNewCallerSeesNextTurnEnd(t *testing.T) {
	f := &fakeSession{state: "running"}
	f.add("turn.completed")
	srv := f.server(t)
	dir := t.TempDir()
	store := CursorStore{Dir: dir}
	ctx, out := waitContext(srv, "fresh", nil)
	if reason, err := waitStart(context.Background(), ctx, "s1", store, []string{"turn_end"}); err != nil || reason != "" {
		t.Fatalf("start: %q %v", reason, err)
	}
	f.add("turn.completed")
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", dir, "--until", "turn_end", "--timeout", "2s"})
	if ExitCode(err) != ExitWaitTurnEnd || !strings.Contains(out.String(), "turn.completed") {
		t.Fatalf("err=%v out=%q", err, out)
	}
	if seq, _ := store.Load("fresh", waitKey("s1")); seq != 2 {
		t.Fatalf("wait cursor = %d", seq)
	}
}

func TestWaitNewCallerReportsFinishedSessionAtOnce(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("turn.completed")
	f.add("session.completed")
	srv := f.server(t)
	ctx, out := waitContext(srv, "late", nil)
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", t.TempDir(), "--until", "approval,error,done", "--timeout", "2s"})
	if ExitCode(err) != ExitCode(WaitReasonError("done")) || !strings.Contains(out.String(), "already completed") {
		t.Fatalf("err=%v out=%q", err, out)
	}
	if len(f.wsAfter) != 0 {
		t.Fatal("finished session should not open a stream")
	}
}

func TestWaitNewCallerReportsFailedSessionAsError(t *testing.T) {
	f := &fakeSession{state: "failed"}
	f.add("turn.failed")
	srv := f.server(t)
	ctx, _ := waitContext(srv, "late", nil)
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", t.TempDir(), "--until", "error", "--timeout", "2s"})
	if ExitCode(err) != ExitCode(WaitReasonError("error")) {
		t.Fatalf("err=%v", err)
	}
}

func TestWaitNewCallerReportsPendingApprovalAtOnce(t *testing.T) {
	f := &fakeSession{state: "running", approvals: 1}
	f.add("control.approval.request")
	srv := f.server(t)
	ctx, out := waitContext(srv, "late", nil)
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", t.TempDir(), "--until", "approval,done", "--timeout", "2s"})
	if ExitCode(err) != ExitCode(WaitReasonError("approval")) || !strings.Contains(out.String(), "waiting for approval") {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

func TestWaitAfterPromptStartsAtControlMark(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("turn.completed")
	f.add("session.completed")
	srv := f.server(t)
	dir := t.TempDir()
	store := CursorStore{Dir: dir}
	ctx, _ := waitContext(srv, "me", &store)
	markControl(ctx, "s1") // what prompt does before sending
	if seq, found, _ := store.Lookup("me", controlKey("s1")); !found || seq != 2 {
		t.Fatalf("control mark = %d %v", seq, found)
	}
	// The session still reads as completed until the wake lands; the caller's
	// wait must not mistake that for the result of its prompt.
	f.add("lifecycle.turn.started")
	f.add("turn.completed")
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", dir, "--until", "turn_end,done", "--timeout", "2s"})
	if ExitCode(err) != ExitWaitTurnEnd {
		t.Fatalf("err=%v code=%d", err, ExitCode(err))
	}
	if f.wsAfter[0] != "2" {
		t.Fatalf("stream after %v", f.wsAfter)
	}
}

func TestControlMarkMovesOldWaitCursorForward(t *testing.T) {
	f := &fakeSession{state: "running"}
	for i := 0; i < 12; i++ {
		f.add("content.delta")
	}
	srv := f.server(t)
	store := CursorStore{Dir: t.TempDir()}
	_ = store.Save("me", waitKey("s1"), 3)
	_ = store.Save("me", controlKey("s1"), 9)
	ctx, _ := waitContext(srv, "me", nil)
	if _, err := waitStart(context.Background(), ctx, "s1", store, []string{"turn_end"}); err != nil {
		t.Fatal(err)
	}
	if seq, _ := store.Load("me", waitKey("s1")); seq != 9 {
		t.Fatalf("wait cursor = %d", seq)
	}
	_ = store.Save("me", waitKey("s1"), 12)
	if _, err := waitStart(context.Background(), ctx, "s1", store, []string{"turn_end"}); err != nil {
		t.Fatal(err)
	}
	if seq, _ := store.Load("me", waitKey("s1")); seq != 12 {
		t.Fatalf("newer wait progress lost: %d", seq)
	}
}

func TestWaitStartWriteFailureKeepsNotice(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("session.completed")
	srv := f.server(t)
	store := CursorStore{Dir: t.TempDir()}
	ctx, _ := waitContext(srv, "late", nil)
	ctx.Out = brokenWriter{}
	if _, err := waitStart(context.Background(), ctx, "s1", store, []string{"done"}); err == nil {
		t.Fatal("expected write error")
	}
	if _, found, _ := store.Lookup("late", waitKey("s1")); found {
		t.Fatal("failed notice consumed the wait")
	}
	out := &strings.Builder{}
	ctx.Out = out
	if reason, err := waitStart(context.Background(), ctx, "s1", store, []string{"done"}); err != nil || reason != "done" {
		t.Fatalf("retry: %q %v", reason, err)
	}
}

func TestWaitWithCursorStillReportsPendingApproval(t *testing.T) {
	f := &fakeSession{state: "running", approvals: 1}
	f.add("control.approval.request")
	srv := f.server(t)
	dir := t.TempDir()
	store := CursorStore{Dir: dir}
	_ = store.Save("me", waitKey("s1"), 1) // an earlier wait already read the request
	ctx, out := waitContext(srv, "me", nil)
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", dir, "--until", "approval,done", "--timeout", "2s"})
	if ExitCode(err) != ExitCode(WaitReasonError("approval")) || !strings.Contains(out.String(), "waiting for approval") {
		t.Fatalf("err=%v out=%q", err, out)
	}
	if seq, _ := store.Load("me", waitKey("s1")); seq != 1 {
		t.Fatalf("cursor moved to %d", seq)
	}
}

func TestWaitWithCursorReportsFinishedSession(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("session.completed")
	srv := f.server(t)
	dir := t.TempDir()
	store := CursorStore{Dir: dir}
	_ = store.Save("me", waitKey("s1"), 1)
	ctx, _ := waitContext(srv, "me", nil)
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", dir, "--until", "done", "--timeout", "2s"})
	if ExitCode(err) != ExitCode(WaitReasonError("done")) {
		t.Fatalf("finished session should not hang: %v", err)
	}
}

func TestResumeMarksBeforeSending(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("turn.completed")
	f.add("session.completed")
	var markedAt int
	inner := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resume") {
			f.mu.Lock()
			markedAt = len(f.events)
			f.mu.Unlock()
			f.add("lifecycle.turn.started")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"session_id": "s1", "state": "starting"}})
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	store := CursorStore{Dir: t.TempDir()}
	ctx, _ := waitContext(srv, "me", &store)
	if err := cmdResume(ctx, []string{"s1", "continue"}); err != nil {
		t.Fatal(err)
	}
	if seq, found, _ := store.Lookup("me", controlKey("s1")); !found || seq != int64(markedAt) {
		t.Fatalf("resume mark = %d %v, want %d", seq, found, markedAt)
	}
}

func TestMarkCreatedReplaysNewSessionOnce(t *testing.T) {
	store := CursorStore{Dir: t.TempDir()}
	ctx := &Context{Config: Config{Caller: "me"}, Cursors: &store}
	markCreated(ctx, "s1")
	if seq, found, _ := store.Lookup("me", controlKey("s1")); !found || seq != 0 {
		t.Fatalf("mark = %d %v", seq, found)
	}
	_ = store.Save("me", controlKey("s1"), 7)
	markCreated(ctx, "s1")
	if seq, _ := store.Load("me", controlKey("s1")); seq != 7 {
		t.Fatalf("existing mark overwritten: %d", seq)
	}
}

func TestWaitDoesNotConsumeSinceCursor(t *testing.T) {
	f := &fakeSession{state: "running"}
	f.add("turn.completed")
	srv := f.server(t)
	dir := t.TempDir()
	store := CursorStore{Dir: dir}
	_ = store.Save("me", controlKey("s1"), 0)
	ctx, _ := waitContext(srv, "me", nil)
	if err := cmdWait(ctx, []string{"s1", "--cursor-dir", dir, "--until", "turn_end", "--timeout", "2s"}); ExitCode(err) != ExitWaitTurnEnd {
		t.Fatalf("err=%v", err)
	}
	if seq, found, _ := store.Lookup("me", "s1"); found {
		t.Fatalf("wait moved the since cursor to %d", seq)
	}
}

func TestControlStoreNeedsExplicitLocation(t *testing.T) {
	if controlStoreFrom(map[string]string{}) != nil {
		t.Fatal("bare environment must not mark controls")
	}
	if s := controlStoreFrom(map[string]string{"AGENTCTL_CURSOR_DIR": "/x"}); s == nil || s.Dir != "/x" {
		t.Fatalf("store = %+v", s)
	}
}

func TestRejectedPromptRollsBackControlMark(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("turn.completed")
	f.add("session.completed")
	inner := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_state","message":"provider state not retained"}}`))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	dir := t.TempDir()
	store := CursorStore{Dir: dir}
	ctx, _ := waitContext(srv, "me", &store)
	if err := cmdInput(ctx, []string{"s1", "continue"}, "prompt"); err == nil {
		t.Fatal("expected the prompt to be rejected")
	}
	if _, found, _ := store.Lookup("me", controlKey("s1")); found {
		t.Fatal("rejected prompt left a control mark")
	}
	_ = store.Save("me", controlKey("s1"), 1)
	if err := cmdInput(ctx, []string{"s1", "again"}, "prompt"); err == nil {
		t.Fatal("expected the prompt to be rejected")
	}
	if seq, _ := store.Load("me", controlKey("s1")); seq != 1 {
		t.Fatalf("previous mark not restored: %d", seq)
	}
	out := &strings.Builder{}
	ctx.Out = out
	_ = store.Delete("me", controlKey("s1"))
	err := cmdWait(ctx, []string{"s1", "--cursor-dir", dir, "--until", "done", "--timeout", "2s"})
	if ExitCode(err) != ExitCode(WaitReasonError("done")) {
		t.Fatalf("finished session hidden after rejected prompt: %v", err)
	}
}
