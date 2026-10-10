package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type approvalFixture struct {
	nativeprotocol.Transport
	pending []nativeprotocol.PendingApproval
	calls   int
	fail    bool
}

func (f *approvalFixture) PendingApprovals() []nativeprotocol.PendingApproval { return f.pending }
func (f *approvalFixture) DecideApproval(context.Context, json.RawMessage, string) error {
	f.calls++
	f.pending = nil
	if f.fail {
		return errors.New("lost response")
	}
	return nil
}
func approvalServer(t *testing.T) (*Server, *approvalFixture, *gin.Engine) {
	t.Helper()
	ctx := context.Background()
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	_, err := st.CreateSession(ctx, durable.CreateSessionParams{SessionID: "s1", IdempotencyKey: "dispatch", RequestHash: "sha256:fixture", RequestManifest: json.RawMessage(`{}`), Agent: "codex", Runtime: "docker", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: "s1", Runtime: "docker", ContainerID: "fixture", ImageReference: "fixture", ImageDigest: "sha256:fixture", SandboxProfile: "fixture", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	f := &approvalFixture{pending: []nativeprotocol.PendingApproval{{RequestID: json.RawMessage(`42`), Method: "item/commandExecution/requestApproval", CanAccept: true, ExpiresAt: time.Now().Add(time.Minute)}}}
	s := &Server{durableStore: st, eventBroker: eventstream.New(st), native: map[string]*activeNativeSession{"s1": newActiveNativeSession(f)}}
	r := gin.New()
	r.GET("/sessions/:id/approvals", s.handleV1PendingApprovals)
	r.POST("/sessions/:id/approvals", s.handleV1ApprovalDecision)
	return s, f, r
}
func TestApprovalHTTPExactScopeAndIdempotency(t *testing.T) {
	_, f, r := approvalServer(t)
	for _, test := range []struct {
		path, body string
		want       int
	}{
		{"other", `{"request_id":42,"decision":"accept","idempotency_key":"wrong"}`, 409},
		{"s1", `{"request_id":{},"decision":"accept","idempotency_key":"object"}`, 400},
		{"s1", `{"request_id":null,"decision":"accept","idempotency_key":"null"}`, 400},
		{"s1", `{"request_id":43,"decision":"accept","idempotency_key":"missing"}`, 409},
		{"s1", `{"request_id":42,"decision":"invalid","idempotency_key":"bad"}`, 400},
		{"s1", `{"request_id":42,"decision":"accept","idempotency_key":"ok"}`, 202},
		{"s1", `{"request_id":42,"decision":"accept","idempotency_key":"ok"}`, 200},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/sessions/"+test.path+"/approvals", strings.NewReader(test.body)))
		if w.Code != test.want {
			t.Fatalf("%s status %d body%s", test.path, w.Code, w.Body.String())
		}
	}
	if f.calls != 1 {
		t.Fatalf("calls %d", f.calls)
	}
}
func TestApprovalUnknownDeliveryNotReplayed(t *testing.T) {
	_, f, r := approvalServer(t)
	f.fail = true
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/sessions/s1/approvals", strings.NewReader(`{"request_id":42,"decision":"accept","idempotency_key":"unknown"}`)))
		if w.Code < 400 {
			t.Fatal(w.Body.String())
		}
	}
	if f.calls != 1 {
		t.Fatal("unknown delivery replayed")
	}
}

func TestApprovalHistoryNeverRecreatesConsent(t *testing.T) {
	_, f, r := approvalServer(t)
	body := `{"request_id":42,"decision":"accept","idempotency_key":"first"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/sessions/s1/approvals", strings.NewReader(body)))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	// An AgentD Docker reattachment can see a prior stdout request. The durable
	// request-ID control protects it even when caller idempotency key changes.
	f.pending = []nativeprotocol.PendingApproval{{RequestID: json.RawMessage(`42`), Method: "item/commandExecution/requestApproval", CanAccept: true, ExpiresAt: time.Now().Add(time.Minute)}}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/sessions/s1/approvals", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"approvals":[]`) {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/sessions/s1/approvals", strings.NewReader(`{"request_id":42,"decision":"decline","idempotency_key":"different"}`)))
	if w.Code < 400 {
		t.Fatal("changed decision accepted")
	}
	if f.calls != 1 {
		t.Fatal("consent replayed")
	}
}
