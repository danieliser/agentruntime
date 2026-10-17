package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestLoadPersistTokenUsesFileAndNeverURL(t *testing.T) {
	path := t.TempDir() + "/persist-token"
	secret := "persist-secret-do-not-print"
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	got, err := LoadPersistToken(map[string]string{"PERSIST_TOKEN_FILE": path})
	if err != nil || got != secret {
		t.Fatalf("token read failed: %v", err)
	}
	if strings.Contains(defaultPersistURL, secret) {
		t.Fatal("token appeared in default URL")
	}
}

func TestPersistResolveTaskAndControlRoutes(t *testing.T) {
	var sawAuth, sawControl bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing PERSIST auth header")
		}
		sawAuth = true
		switch r.URL.Path {
		case "/api/v1/tasks/task-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": "session-1"})
		case "/api/v1/sessions/session-1/prompt":
			sawControl = true
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"accepted":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("PERSIST_TOKEN_FILE", t.TempDir()+"/token")
	_ = os.WriteFile(os.Getenv("PERSIST_TOKEN_FILE"), []byte("test-token"), 0600)
	p := NewPersistClient(Config{PersistURL: srv.URL})
	ref := SessionReference{Kind: "task", ID: "task-1"}
	session, err := p.resolve(ref)
	if err != nil || session != "session-1" {
		t.Fatalf("resolve = %q, %v", session, err)
	}
	ok, err := p.controlSession(session, "prompt", "follow up")
	if err != nil || !ok || !sawAuth || !sawControl {
		t.Fatalf("control = %v, %v; auth=%v route=%v", ok, err, sawAuth, sawControl)
	}
}

func TestPlanWaitReasonsAndSummaryFields(t *testing.T) {
	if got := planWaitReason(planEvent{Type: "approval_requested"}); got != "approval" {
		t.Fatalf("reason=%q", got)
	}
	if got := planWaitReason(planEvent{Type: "plan_status_changed", NewStatus: "completed"}); got != "done" {
		t.Fatalf("reason=%q", got)
	}
	e := planEvent{Type: "node_started", TaskID: "node", SessionID: "session"}
	if e.TaskID != "node" || e.SessionID != "session" {
		t.Fatal("plan event fields not decoded")
	}
}

func TestPlanWaitExitCodesAreDistinct(t *testing.T) {
	cases := map[string]int{"node_started": ExitWaitNodeStarted, "approval": ExitWaitApproval, "node_done": ExitWaitNodeDone, "failed": ExitWaitFailed, "done": ExitWaitDone}
	seen := map[int]bool{}
	for reason, want := range cases {
		got := ExitCode(WaitReasonError(reason))
		if got != want {
			t.Errorf("%s exit=%d want=%d", reason, got, want)
		}
		if seen[got] {
			t.Errorf("duplicate exit code %d", got)
		}
		seen[got] = true
	}
}

func TestPlanNodeReferenceResolvesByTitleSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/plans/plan-1" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"plan":{"id":"plan-1"},"graph":{"planId":"plan-1","nodes":[{"id":"task-1","title":"Build Agent CLI","status":"running","batchId":"batch-1","session_id":"sess-1","pending_approvals":2,"blocked_on":[]}]}}`))
	}))
	defer srv.Close()
	path := t.TempDir() + "/token"
	if e := os.WriteFile(path, []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PERSIST_TOKEN_FILE", path)
	p := NewPersistClient(Config{PersistURL: srv.URL})
	ref := SessionReference{Kind: "plan_node", ID: "plan-1", Node: "build-agent-cli"}
	got, e := p.resolve(ref)
	if e != nil || got != "sess-1" {
		t.Fatalf("resolve=%q err=%v", got, e)
	}
}

func TestPersistApprovalListUsesBrokerIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/session-1/approvals" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		w.Write([]byte(`{"approvals":[{"id":"broker-id","session_id":"session-1"}]}`))
	}))
	defer srv.Close()
	p := NewPersistClient(Config{PersistURL: srv.URL, PersistToken: "private"})
	ctx := &Context{Persist: p, Reference: &SessionReference{Kind: "session", ID: "session-1"}}
	list, err := approvals(ctx, "session-1")
	if err != nil || len(list.Approvals) != 1 || !strings.Contains(string(list.Approvals[0]), "broker-id") {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}

func TestHiddenPersistSessionCannotFallBackToDirect(t *testing.T) {
	directCalls := 0
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":{"session_id":"owned","task_id":"persist-task","state":"completed"}}`))
			return
		}
		directCalls++
		w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer agent.Close()
	persist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer persist.Close()
	ctx, out := commandContext(agent, true)
	ctx.Out = out
	ctx.Persist = NewPersistClient(Config{PersistURL: persist.URL, PersistToken: "private"})
	ctx.Reference = &SessionReference{Kind: "session", ID: "owned"}
	if err := cmdInput(ctx, []string{"owned", "continue"}, "prompt"); err == nil {
		t.Error("hidden owned session accepted via direct AgentD")
	}
	if directCalls != 0 {
		t.Errorf("direct calls=%d", directCalls)
	}
}

func TestWaitAcceptsTaskReferences(t *testing.T) {
	if _, ok := sessionArgument("wait"); !ok {
		t.Fatal("wait does not resolve task references")
	}
}

func TestStandaloneControlAfterNegativePersistLookup(t *testing.T) {
	writes := 0
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":{"session_id":"standalone","state":"running"}}`))
			return
		}
		writes++
		w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer agent.Close()
	ctx, _ := commandContext(agent, true)
	ctx.Reference = &SessionReference{Kind: "session", ID: "standalone"}
	persist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer persist.Close()
	ctx.Persist = NewPersistClient(Config{PersistURL: persist.URL, PersistToken: "fixture"})
	if err := cmdInput(ctx, []string{"standalone", "steer"}, "steer"); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("writes=%d", writes)
	}
}

func TestPlanShowPreservesNullUndispatchedSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"graph":{"nodes":[{"id":"b","title":"B","status":"pending","session_id":null,"blocked_on":["a"]}]}}`))
	}))
	defer srv.Close()
	ctx, out := commandContext(srv, true)
	ctx.Persist = NewPersistClient(Config{PersistURL: srv.URL, PersistToken: "private"})
	if err := cmdPlan(ctx, []string{"show", "p"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"session_id":null`) {
		t.Fatalf("lost null: %s", out)
	}
}

func TestPersistApprovalAcceptsQuotedStringID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/s/approvals/broker-id/approve" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"accepted":true}`))
	}))
	defer srv.Close()
	p := NewPersistClient(Config{PersistURL: srv.URL, PersistToken: "private"})
	if _, err := p.controlSession("s", "approve", `s "broker-id"`); err != nil {
		t.Fatal(err)
	}
}

func TestTaskOwnedLifecycleRequiresExplicitDirect(t *testing.T) {
	for _, command := range []string{"cancel", "terminate", "resume"} {
		t.Run(command, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Write([]byte(`{"data":{"session_id":"s","task_id":"persist-task","state":"running"}}`))
					return
				}
				writes++
				w.Write([]byte(`{"data":{"accepted":true}}`))
			}))
			defer srv.Close()
			ctx, _ := commandContext(srv, true)
			ctx.Persist = NewPersistClient(Config{PersistURL: srv.URL, PersistToken: "private"})
			var err error
			if command == "resume" {
				err = cmdResume(ctx, []string{"s", "continue"})
			} else {
				err = control(ctx, []string{"s"}, command)
			}
			if err == nil || writes != 0 {
				t.Fatalf("unapproved direct lifecycle action: err=%v writes=%d", err, writes)
			}
		})
	}
}
