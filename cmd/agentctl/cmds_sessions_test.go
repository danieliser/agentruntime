package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func commandContext(ts *httptest.Server, jsonMode bool) (*Context, *bytes.Buffer) {
	out := new(bytes.Buffer)
	c := NewAPIClient(Config{URL: ts.URL, Token: "test-token"})
	return &Context{Config: Config{URL: ts.URL, Token: "test-token", JSON: jsonMode}, API: c, Out: out, Command: ""}, out
}

func TestRunDispatchesAndPrintsSessionID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("missing bearer auth")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["agent"] != "claude" || body["model"] != "M" || body["effort"] != "high" || body["work_dir"] != "/repo" || body["runtime"] != "docker" || body["prompt"] != "fix it" {
			t.Fatalf("dispatch body %#v", body)
		}
		_, _ = w.Write([]byte(`{"data":{"session_id":"sess-1","state":"created"}}`))
	}))
	defer ts.Close()
	ctx, out := commandContext(ts, true)
	if err := cmdRun(ctx, []string{"--provider", "claude", "--model", "M", "--effort", "high", "--cwd", "/repo", "--runtime", "docker", "fix it"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"session_id":"sess-1"`) {
		t.Fatalf("output %q", out.String())
	}
}

func TestStatusAndGrepReadDurableHistory(t *testing.T) {
	event := map[string]any{"schema_version": "v1", "event_id": "e1", "session_id": "s1", "generation": 1, "sequence": 1, "type": "turn.started", "payload": json.RawMessage(`{"turn_id":"turn-7"}`), "raw_base64": base64.StdEncoding.EncodeToString([]byte("turn")), "raw_sha256": "sha256:x"}
	encoded, _ := json.Marshal(event)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions/s1":
			_, _ = w.Write([]byte(`{"data":{"session_id":"s1","state":"running","last_sequence":1}}`))
		case "/api/v1/sessions/s1/approvals":
			_, _ = w.Write([]byte(`{"data":{"session_id":"s1","approvals":[{},{}]}}`))
		case "/api/v1/sessions/s1/events":
			_, _ = w.Write([]byte(`{"data":{"events":[` + string(encoded) + `],"last_sequence":1,"has_more":false}}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.String())
		}
	}))
	defer ts.Close()
	ctx, out := commandContext(ts, false)
	if err := cmdStatus(ctx, []string{"s1"}); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(out.String())
	if strings.Contains(line, "\n") || !strings.Contains(line, "turn=turn-7") || !strings.Contains(line, "approvals=2") || !strings.Contains(line, "seq=1") {
		t.Fatalf("status line %q", line)
	}
	out.Reset()
	ctx.Config.JSON = true
	if err := cmdGrep(ctx, []string{"s1", "turn", "--type", "turn.started"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"event_id":"e1"`) {
		t.Fatalf("grep output %q", out.String())
	}
}

func TestApprovalDecisionUsesExistingEndpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/s1" {
			_, _ = w.Write([]byte(`{"data":{"session_id":"s1","state":"running"}}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/s1/approvals" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RequestID json.RawMessage `json:"request_id"`
			Decision  string          `json:"decision"`
			Key       string          `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body.RequestID) != "42" || body.Decision != "accept" || body.Key == "" {
			t.Fatalf("approval body %+v", body)
		}
		_, _ = w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer ts.Close()
	ctx, _ := commandContext(ts, false)
	if err := cmdApprove(ctx, []string{"s1", "42"}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandRegistryUsesImplementedHandlers(t *testing.T) {
	for _, name := range []string{"run", "status", "grep", "steer", "prompt", "interrupt", "cancel", "terminate", "resume", "approvals", "approve", "deny", "result", "ls"} {
		if commandRegistry[name] == nil || reflect.ValueOf(commandRegistry[name]).Pointer() == reflect.ValueOf(skeletonHandler).Pointer() {
			t.Errorf("%s is not registered", name)
		}
	}
}

func TestStatusUsesNativeProviderTurnIdentity(t *testing.T) {
	for _, tc := range []struct{ name, events, want string }{
		{"codex", `[{"sequence":1,"type":"lifecycle.turn.started","payload":{},"raw_base64":"` + base64.StdEncoding.EncodeToString([]byte(`{"params":{"turn":{"id":"native-turn"}}}`)) + `"}]`, "native-turn"},
		{"claude", `[{"sequence":1,"type":"content.completed","payload":{"text":"first"}},{"sequence":2,"type":"control.prompt.dispatched","payload":{}}]`, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					fmt.Fprintf(w, `{"data":{"events":%s}}`, tc.events)
					return
				}
				fmt.Fprint(w, `{"data":{"session_id":"s1","state":"completed","last_sequence":2}}`)
			}))
			defer srv.Close()
			ctx, out := commandContext(srv, false)
			if err := cmdStatus(ctx, []string{"s1"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "turn="+tc.want) {
				t.Fatalf("got %s", out.String())
			}
		})
	}
}

func TestStatusExposesWakeState(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			w.Write([]byte(`{"data":{"events":[],"has_more":false}}`))
			return
		}
		w.Write([]byte(`{"data":{"session_id":"s1","state":"completed","generation":2,"continuable":true,"continuable_reason":"native_resume_available","runtime":"docker"}}`))
	}))
	defer ts.Close()
	ctx, out := commandContext(ts, true)
	if err := cmdStatus(ctx, []string{"s1"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["generation"] != float64(2) || got["continuable"] != true || got["runtime"] != "docker" {
		t.Fatalf("missing wake state %s", out)
	}
}
