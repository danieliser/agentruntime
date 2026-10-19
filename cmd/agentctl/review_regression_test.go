package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParsePreservesLiteralGlobalNamesAndDelimiter(t *testing.T) {
	for _, args := range [][]string{{"prompt", "s1", "json"}, {"prompt", "s1", "--", "--json"}} {
		got, err := ParseArgs(args, nil)
		want := []string{"s1", "json"}
		if len(args) == 4 {
			want = []string{"s1", "--", "--json"}
		}
		if err != nil || got.Config.JSON || !reflect.DeepEqual(got.Args, want) {
			t.Fatalf("%v => %+v %v", args, got, err)
		}
	}
}

func TestWaitJSONAndUnauthorized(t *testing.T) {
	for _, unauthorized := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "auth"}[unauthorized], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if unauthorized {
					w.WriteHeader(401)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.WriteJSON(map[string]any{"sequence": 1, "type": "turn.completed", "payload": map[string]string{"status": "interrupted"}})
			}))
			defer srv.Close()
			var out bytes.Buffer
			err := waitStream(context.Background(), Config{URL: srv.URL, Caller: "review", JSON: true}, "s1", CursorStore{Dir: t.TempDir()}, []string{"turn_end"}, 100*time.Millisecond, &out)
			if unauthorized {
				if !errors.Is(err, ErrAuth) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if ExitCode(err) != 10 || !json.Valid(out.Bytes()) {
				t.Fatalf("err=%v output=%s", err, out.String())
			}
		})
	}
}

func TestSameInputCanBeSubmittedOnDifferentTurns(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		keys = append(keys, body.Key)
		_, _ = w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer srv.Close()
	ctx, _ := commandContext(srv, true)
	for i := 0; i < 2; i++ {
		if err := cmdPrompt(ctx, []string{"s1", "again"}); err != nil {
			t.Fatal(err)
		}
	}
	if keys[0] == keys[1] {
		t.Fatal("independent turns deduplicated as the same request")
	}
}

func TestSinceJSONAndOutputFailureDoesNotAdvanceCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"events":[{"sequence":1,"type":"content.completed","payload":{"text":"hello"}}],"last_sequence":1}}`))
	}))
	defer srv.Close()
	ctx, out := commandContext(srv, true)
	ctx.Config.Caller = "review"
	dir := t.TempDir()
	if err := cmdSince(ctx, []string{"s1", "--cursor-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("invalid JSON %s", out.String())
	}
	ctx.Out = brokenWriter{}
	dir = t.TempDir()
	if err := cmdSince(ctx, []string{"s1", "--cursor-dir", dir}); err == nil {
		t.Fatal("expected output failure")
	}
	seq, err := (CursorStore{Dir: dir}).Load("review", "s1")
	if err != nil || seq != 0 {
		t.Fatalf("cursor advanced despite output failure: %d %v", seq, err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write(b []byte) (int, error) { return 0, errors.New("closed output") }

func TestWaitOutputFailureDoesNotConsumeNotification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"sequence": 1, "type": "turn.completed"})
	}))
	defer srv.Close()
	store := CursorStore{Dir: t.TempDir()}
	err := waitStream(context.Background(), Config{URL: srv.URL, Caller: "review", JSON: true}, "s1", store, []string{"turn_end"}, time.Second, brokenWriter{})
	if err == nil {
		t.Fatal("expected writer error")
	}
	seq, err := store.Load("review", waitKey("s1"))
	if err != nil || seq != 0 {
		t.Fatalf("notification consumed: %d %v", seq, err)
	}
}
