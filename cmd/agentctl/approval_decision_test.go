package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRawSessionApprovalPreservesQuotedStringID(t *testing.T) {
	for _, decision := range []string{"approve", "deny"} {
		for _, id := range []string{`request "one"`, `"request \"one\""`} {
			t.Run(decision+"/"+id, func(t *testing.T) {
				posts := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/raw-session" {
						w.Write([]byte(`{"data":{"session_id":"raw-session","state":"running"}}`))
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/raw-session/approvals" {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						w.WriteHeader(500)
						return
					}
					posts++
					var body struct {
						RequestID string `json:"request_id"`
						Decision  string `json:"decision"`
						Key       string `json:"idempotency_key"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					wantDecision := "accept"
					if decision == "deny" {
						wantDecision = "decline"
					}
					if body.RequestID != `request "one"` || body.Decision != wantDecision || body.Key == "" {
						t.Errorf("approval body: %+v", body)
					}
					w.Write([]byte(`{"data":{"accepted":true}}`))
				}))
				defer srv.Close()
				ctx, _ := commandContext(srv, true)
				if err := decide(ctx, []string{"raw-session", id}, decision); err != nil {
					t.Fatal(err)
				}
				if posts != 1 {
					t.Fatalf("approval posts=%d", posts)
				}
			})
		}
	}
}
