package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistPrefersCurrentSessionWithLegacyFallback(t *testing.T) {
	for _, current := range []string{"supervised", ""} {
		t.Run(current, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/tasks/t" {
					json.NewEncoder(w).Encode(map[string]any{"session_id": "original", "current_session_id": current})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"graph": map[string]any{"nodes": []map[string]any{{"id": "t", "title": "Node", "session_id": "original", "current_session_id": current}}}})
			}))
			defer srv.Close()
			p := NewPersistClient(Config{PersistURL: srv.URL, PersistToken: "fixture"})
			want := current
			if want == "" {
				want = "original"
			}
			for _, ref := range []SessionReference{{Kind: "task", ID: "t"}, {Kind: "plan_node", ID: "p", Node: "t"}, {Kind: "plan", ID: "p"}} {
				got, err := p.resolve(ref)
				if err != nil || got != want {
					t.Fatalf("%+v got=%s err=%v", ref, got, err)
				}
			}
		})
	}
}

func TestManifestWithoutTaskStillChecksPersistOwnership(t *testing.T) {
	for _, status := range []int{200, 403, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			writes := 0
			routed := 0
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.Write([]byte(`{"data":{"session_id":"s","state":"running"}}`))
					return
				}
				writes++
				w.Write([]byte(`{"data":{"accepted":true}}`))
			}))
			defer agent.Close()
			persist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(status)
					w.Write([]byte(`{"approvals":[]}`))
					return
				}
				routed++
				w.Write([]byte(`{}`))
			}))
			defer persist.Close()
			ctx, _ := commandContext(agent, true)
			ctx.Persist = NewPersistClient(Config{PersistURL: persist.URL, PersistToken: "fixture"})
			err := cmdInput(ctx, []string{"s", "next"}, "prompt")
			if writes != 0 {
				t.Fatal("bypassed PERSIST ownership")
			}
			if status == 200 && (err != nil || routed != 1) {
				t.Fatalf("routed=%d err=%v", routed, err)
			}
			if status != 200 && err == nil {
				t.Fatal("lookup failure did not fail closed")
			}
		})
	}
}

func TestLifecycleWithoutTaskStillChecksPersistOwnership(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"session_id":"s","state":"running"}}`))
	}))
	defer agent.Close()
	persist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"approvals":[]}`)) }))
	defer persist.Close()
	ctx, _ := commandContext(agent, true)
	ctx.Persist = NewPersistClient(Config{PersistURL: persist.URL, PersistToken: "fixture"})
	if err := requireDirectForTaskLifecycle(ctx, "s"); err == nil {
		t.Fatal("owned lifecycle allowed without --direct")
	}
}

func TestDirectFlagPreservesPromptText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	original := commandRegistry["prompt"]
	defer func() { commandRegistry["prompt"] = original }()
	for _, tc := range []struct {
		args   []string
		direct bool
		last   string
	}{
		{[]string{"prompt", "s", "--direct"}, false, "--direct"},
		{[]string{"prompt", "s", "--", "--direct"}, false, "--direct"},
		{[]string{"--direct", "prompt", "s", "text"}, true, "text"},
		{[]string{"prompt", "--direct", "s", "text"}, true, "text"},
	} {
		called := false
		commandRegistry["prompt"] = func(ctx *Context, args []string) error {
			called = true
			if ctx.Direct != tc.direct || args[len(args)-1] != tc.last {
				t.Errorf("%v direct=%v args=%v", tc.args, ctx.Direct, args)
			}
			return nil
		}
		var out bytes.Buffer
		if code := run(tc.args, &out, &out, map[string]string{"AGENTD_TOKEN_FILE": path}); code != 0 || !called {
			t.Fatalf("%v code=%d output=%s", tc.args, code, out.String())
		}
	}
}
