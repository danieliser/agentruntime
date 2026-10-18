package main

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
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

// deadPersistURL returns a URL nothing listens on.
func deadPersistURL(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func standaloneContext(t *testing.T, agent *httptest.Server, persist Config) *Context {
	ctx, _ := commandContext(agent, true)
	ctx.Reference = &SessionReference{Kind: "session", ID: "s"}
	ctx.Persist = NewPersistClient(persist, map[string]string{"HOME": t.TempDir(), "PERSIST_TOKEN_FILE": t.TempDir() + "/missing"})
	return ctx
}

func TestPlainSessionControlsWorkWithoutPersist(t *testing.T) {
	persists := map[string]Config{
		"unreachable": {PersistURL: deadPersistURL(t), PersistToken: "fixture"},
		"no token":    {PersistURL: deadPersistURL(t)},
	}
	for name, persist := range persists {
		t.Run(name, func(t *testing.T) {
			writes := 0
			agent := fakeAgentD(t, `{"session_id":"s","state":"running"}`, &writes)
			for _, kind := range []string{"steer", "prompt"} {
				if err := cmdInput(standaloneContext(t, agent, persist), []string{"s", "go"}, kind); err != nil {
					t.Fatalf("%s: %v", kind, err)
				}
			}
			for _, command := range []string{"interrupt", "cancel", "terminate"} {
				if err := control(standaloneContext(t, agent, persist), []string{"s"}, command); err != nil {
					t.Fatalf("%s: %v", command, err)
				}
			}
			if err := decide(standaloneContext(t, agent, persist), []string{"s", "req-1"}, "approve"); err != nil {
				t.Fatalf("approve: %v", err)
			}
			if _, err := approvals(standaloneContext(t, agent, persist), "s"); err != nil {
				t.Fatalf("approvals: %v", err)
			}
			if writes != 6 {
				t.Fatalf("direct writes=%d want 6", writes)
			}
		})
	}
}

func TestPersistManagedSessionRefusesDirectWhenPersistDown(t *testing.T) {
	writes := 0
	agent := fakeAgentD(t, `{"session_id":"s","state":"running","tags":{"managed_by":"persist"}}`, &writes)
	ctx := standaloneContext(t, agent, Config{PersistURL: deadPersistURL(t), PersistToken: "fixture"})
	err := cmdInput(ctx, []string{"s", "go"}, "steer")
	if err == nil || !strings.Contains(err.Error(), "--direct") || ExitCode(err) != ExitAPI {
		t.Fatalf("err=%v exit=%d", err, ExitCode(err))
	}
	if err := control(standaloneContext(t, agent, Config{PersistURL: deadPersistURL(t)}), []string{"s"}, "cancel"); err == nil {
		t.Fatal("managed cancel allowed without --direct")
	}
	if writes != 0 {
		t.Fatalf("direct writes=%d", writes)
	}
	direct := standaloneContext(t, agent, Config{PersistURL: deadPersistURL(t)})
	direct.Direct = true
	if err := cmdInput(direct, []string{"s", "go"}, "steer"); err != nil || writes != 1 {
		t.Fatalf("--direct steer: err=%v writes=%d", err, writes)
	}
}

func TestPersistManagedSessionRoutesWithoutOwnershipProbe(t *testing.T) {
	writes, probes, routed := 0, 0, 0
	agent := fakeAgentD(t, `{"session_id":"s","state":"running","tags":{"managed_by":"persist"}}`, &writes)
	persist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			probes++
			w.Write([]byte(`{"approvals":[]}`))
			return
		}
		routed++
		w.Write([]byte(`{}`))
	}))
	defer persist.Close()
	if err := cmdInput(standaloneContext(t, agent, Config{PersistURL: persist.URL, PersistToken: "fixture"}), []string{"s", "go"}, "steer"); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || routed != 1 || probes != 0 {
		t.Fatalf("writes=%d routed=%d probes=%d", writes, routed, probes)
	}
}

func TestOtherOwnerTaskSessionIsControlledDirectlyUnlessPersistOwnsIt(t *testing.T) {
	view := `{"session_id":"s","task_id":"tf:execution_1","state":"running","tags":{"owner":"trading-floor"}}`
	cases := map[string]struct {
		persist        func(t *testing.T) string
		direct, routed int
	}{
		"persist disowns": {persist: func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
			t.Cleanup(srv.Close)
			return srv.URL
		}, direct: 2},
		"persist down": {persist: deadPersistURL, direct: 2},
		// A pre-stamp PERSIST session can carry a caller owner tag.
		"persist owns": {persist: func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"approvals":[]}`)) }))
			t.Cleanup(srv.Close)
			return srv.URL
		}, routed: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			writes := 0
			agent := fakeAgentD(t, view, &writes)
			cfg := Config{PersistURL: tc.persist(t), PersistToken: "fixture"}
			steerErr := cmdInput(standaloneContext(t, agent, cfg), []string{"s", "go"}, "steer")
			cancelErr := control(standaloneContext(t, agent, cfg), []string{"s"}, "cancel")
			if tc.routed > 0 {
				if steerErr != nil || cancelErr == nil || writes != 0 {
					t.Fatalf("owned: steer=%v cancel=%v direct writes=%d", steerErr, cancelErr, writes)
				}
				return
			}
			if steerErr != nil || cancelErr != nil || writes != tc.direct {
				t.Fatalf("steer=%v cancel=%v writes=%d", steerErr, cancelErr, writes)
			}
		})
	}
}

func TestLegacyTaskSessionRefusedWhenPersistDown(t *testing.T) {
	writes := 0
	agent := fakeAgentD(t, `{"session_id":"s","task_id":"persist-task","state":"running"}`, &writes)
	for _, cfg := range []Config{{PersistURL: deadPersistURL(t), PersistToken: "fixture"}, {PersistURL: deadPersistURL(t)}} {
		err := cmdInput(standaloneContext(t, agent, cfg), []string{"s", "go"}, "prompt")
		if err == nil || !strings.Contains(err.Error(), "--direct") {
			t.Fatalf("err=%v", err)
		}
	}
	if writes != 0 {
		t.Fatalf("direct writes=%d", writes)
	}
}

func TestPersistUnavailableClassification(t *testing.T) {
	_, err := NewPersistClient(Config{PersistURL: "http://x"}, map[string]string{"PERSIST_TOKEN_FILE": t.TempDir() + "/missing"}).token()
	if !persistUnavailable(err) || !errors.Is(err, ErrAuth) {
		t.Fatalf("missing token: %v", err)
	}
	unreadable := t.TempDir() + "/token"
	if err := os.WriteFile(unreadable, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistClient(Config{}, map[string]string{"PERSIST_TOKEN_FILE": unreadable}).token(); err == nil || persistUnavailable(err) {
		t.Fatalf("unreadable token treated as absent: %v", err)
	}
	refused := &url.Error{Op: "Get", URL: "http://127.0.0.1:1", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	if !persistUnavailable(refused) {
		t.Fatal("connection refused not treated as absent")
	}
	timeout := &url.Error{Op: "Get", URL: "http://127.0.0.1:1", Err: &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}}
	if persistUnavailable(timeout) {
		t.Fatal("timeout treated as absent")
	}
	if persistUnavailable(&url.Error{Op: "Get", URL: "https://x", Err: errors.New("tls: bad certificate")}) {
		t.Fatal("TLS failure treated as absent")
	}
	if persistUnavailable(errors.New("PERSIST request failed with status 500")) {
		t.Fatal("HTTP refusal treated as absent")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestStatusDegradesWhenPersistDown(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			w.Write([]byte(`{"data":{"events":[],"last_sequence":0}}`))
			return
		}
		w.Write([]byte(`{"data":{"session_id":"s","state":"running","tags":{"managed_by":"persist"}}}`))
	}))
	defer agent.Close()
	ctx := standaloneContext(t, agent, Config{PersistURL: deadPersistURL(t), PersistToken: "fixture"})
	if err := cmdStatus(ctx, []string{"s"}); err != nil {
		t.Fatal(err)
	}
	if out := ctx.Out.(*bytes.Buffer).String(); !strings.Contains(out, `"approvals_available":false`) || !strings.Contains(out, `"pending_approvals":null`) {
		t.Fatalf("approvals not marked unavailable: %s", out)
	}
}
