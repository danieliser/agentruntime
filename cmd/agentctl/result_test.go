package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResultFallsBackToReceiptAndFinalAssistantText(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/result"):
					w.WriteHeader(404)
					fmt.Fprint(w, `{"error":{"code":"NOT_FOUND","message":"structured result is not available"}}`)
				case strings.HasSuffix(r.URL.Path, "/receipt"):
					fmt.Fprint(w, `{"data":{"session_id":"s1","state":"completed","reason":"completed","last_sequence":5}}`)
				case strings.HasSuffix(r.URL.Path, "/events"):
					if r.URL.Query().Get("after_sequence") == "0" {
						fmt.Fprint(w, `{"data":{"events":[{"sequence":1,"type":"turn.started","payload":{}},{"sequence":2,"type":"content.delta","payload":{"text":"old"}},{"sequence":3,"type":"turn.completed","payload":{}}],"last_sequence":3,"has_more":true}}`)
					} else {
						event := `{"sequence":5,"type":"content.delta","payload":{"text":"final reply"}}`
						if provider == "codex" {
							event = `{"sequence":5,"type":"tool.result","payload":{"item":{"type":"agentMessage","text":"final reply"}}}`
						}
						boundary := "lifecycle.turn.started"
						if provider == "claude" {
							boundary = "control.prompt.dispatched"
						}
						fmt.Fprintf(w, `{"data":{"events":[{"sequence":4,"type":"%s","payload":{}},%s],"last_sequence":5,"has_more":false}}`, boundary, event)
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer ts.Close()
			ctx, out := commandContext(ts, true)
			if err := cmdResult(ctx, []string{"s1"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), `"receipt":`) || !strings.Contains(out.String(), `"final_assistant_text":"final reply"`) || strings.Contains(out.String(), "old") {
				t.Fatalf("got %s", out.String())
			}
		})
	}
}

func TestResultDoesNotHideServerFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/result") {
			t.Error("unexpected fallback")
		}
		w.WriteHeader(500)
		fmt.Fprint(w, "failure")
	}))
	defer ts.Close()
	ctx, _ := commandContext(ts, true)
	if err := cmdResult(ctx, []string{"s1"}); err == nil {
		t.Fatal("expected failure")
	}
}
