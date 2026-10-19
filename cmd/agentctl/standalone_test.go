package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeAgentD serves one session view on GET and counts direct writes.
func fakeAgentD(t *testing.T, view string, writes *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if strings.HasSuffix(r.URL.Path, "/approvals") {
				w.Write([]byte(`{"data":{"session_id":"s","approvals":[]}}`))
				return
			}
			w.Write([]byte(`{"data":` + view + `}`))
			return
		}
		*writes++
		w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cliEnv(t *testing.T, url string) map[string]string {
	t.Helper()
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"AGENTD_URL": url, "AGENTD_TOKEN_FILE": path, "AGENTCTL_CURSOR_DIR": t.TempDir()}
}

func TestManagedControlsRequireForce(t *testing.T) {
	for _, command := range []string{"prompt", "steer", "approve", "deny", "cancel", "interrupt", "terminate", "resume"} {
		t.Run(command, func(t *testing.T) {
			writes := 0
			srv := fakeAgentD(t, `{"session_id":"s","state":"running","tags":{"managed_by":"scheduler"}}`, &writes)
			env := cliEnv(t, srv.URL)
			args := []string{command, "s"}
			switch command {
			case "prompt", "steer", "resume":
				args = append(args, "continue")
			case "approve", "deny":
				args = append(args, "request-1")
			}
			var out, log bytes.Buffer
			code := run(args, &out, &log, env)
			if code != ExitUsage || strings.Contains(log.String(), "API error") || !strings.Contains(log.String(), "schedulerctl") || !strings.Contains(log.String(), "--force") || writes != 0 {
				t.Fatalf("code=%d writes=%d log=%s", code, writes, &log)
			}
			store := CursorStore{Dir: env["AGENTCTL_CURSOR_DIR"]}
			if _, found, err := store.Lookup("agentctl", controlKey("s")); err != nil || found {
				t.Fatalf("refusal changed control mark: found=%v err=%v", found, err)
			}
			out.Reset()
			log.Reset()
			if code = run(append([]string{"--force"}, args...), &out, &log, env); code != 0 || writes != 1 {
				t.Fatalf("force code=%d writes=%d log=%s", code, writes, &log)
			}
		})
	}
}

func TestUnmanagedControlsStayDirect(t *testing.T) {
	for _, view := range []string{`{"session_id":"s","state":"running"}`, `{"session_id":"s","state":"running","task_id":"external-task","tags":{"owner":"other-app"}}`} {
		writes := 0
		srv := fakeAgentD(t, view, &writes)
		ctx, _ := commandContext(srv, true)
		for _, kind := range []string{"steer", "prompt"} {
			if err := cmdInput(ctx, []string{"s", "go"}, kind); err != nil {
				t.Fatal(err)
			}
		}
		for _, kind := range []string{"cancel", "interrupt", "terminate"} {
			if err := control(ctx, []string{"s"}, kind); err != nil {
				t.Fatal(err)
			}
		}
		for _, kind := range []string{"approve", "deny"} {
			if err := decide(ctx, []string{"s", "request"}, kind); err != nil {
				t.Fatal(err)
			}
		}
		if err := cmdResume(ctx, []string{"s", "continue"}); err != nil {
			t.Fatal(err)
		}
		if writes != 8 {
			t.Fatalf("writes=%d", writes)
		}
	}
}

func TestManagedReadsAndMarkStayDirect(t *testing.T) {
	view := `{"session_id":"s","state":"completed","last_sequence":1,"tags":{"managed_by":"scheduler"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/sessions":
			w.Write([]byte(`{"data":[` + view + `]}`))
		case strings.HasSuffix(r.URL.Path, "/approvals"):
			w.Write([]byte(`{"data":{"session_id":"s","approvals":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Write([]byte(`{"data":{"events":[{"sequence":1,"type":"content.completed","payload":{"text":"hello"}}],"has_more":false}}`))
		case strings.HasSuffix(r.URL.Path, "/result"):
			w.WriteHeader(404)
		case strings.HasSuffix(r.URL.Path, "/receipt"):
			w.Write([]byte(`{"data":{"session_id":"s","state":"completed","reason":"completed","last_sequence":1}}`))
		default:
			w.Write([]byte(`{"data":` + view + `}`))
		}
	}))
	defer srv.Close()
	env := cliEnv(t, srv.URL)
	for _, args := range [][]string{{"status", "s"}, {"since", "s", "--cursor-dir", env["AGENTCTL_CURSOR_DIR"]}, {"grep", "s", "hello"}, {"wait", "s", "--cursor-dir", env["AGENTCTL_CURSOR_DIR"], "--until", "done", "--timeout", "1s"}, {"result", "s"}, {"approvals", "s"}, {"ls"}, {"mark", "s", "--as", "reader"}} {
		var out, log bytes.Buffer
		code := run(append(args, "--json"), &out, &log, env)
		want := ExitSuccess
		if args[0] == "wait" {
			want = ExitWaitDone
		}
		if code != want || out.Len() == 0 {
			t.Fatalf("%v code=%d out=%s log=%s", args, code, &out, &log)
		}
	}
}

func TestControlLookupFailureBlocksWrites(t *testing.T) {
	for _, status := range []int{401, 404, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Error("write after failed lookup")
			}
			w.WriteHeader(status)
		}))
		ctx, _ := commandContext(srv, true)
		err := cmdInput(ctx, []string{"s", "continue"}, "prompt")
		srv.Close()
		want := map[int]int{401: ExitAuth, 404: ExitNotFound, 500: ExitAPI}[status]
		if ExitCode(err) != want {
			t.Fatalf("status=%d err=%v code=%d", status, err, ExitCode(err))
		}
	}
}
