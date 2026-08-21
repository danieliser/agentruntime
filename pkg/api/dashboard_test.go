package api

import (
	"bytes"
	"net/http"
	"testing"
)

func TestDashboardRedirect(t *testing.T) {
	ts, _ := newTestServer(t)

	// Test redirect from / to /dashboard/
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("Expected 301 redirect, got %d", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if location != "/dashboard/" {
		t.Errorf("Expected redirect to /dashboard/, got %s", location)
	}
}

func TestEmbeddedDashboardUsesDurableV1SessionSurfaces(t *testing.T) {
	app, err := dashboardFS.ReadFile("dashboard/app.js")
	if err != nil {
		t.Fatalf("read embedded dashboard: %v", err)
	}
	for _, required := range [][]byte{[]byte("/api/v1/sessions"), []byte("/api/v1/ws/sessions/")} {
		if !bytes.Contains(app, required) {
			t.Fatalf("dashboard missing durable route %q", required)
		}
	}
	for _, legacy := range [][]byte{
		[]byte("fetch('/sessions')"), []byte("/sessions/history"),
		[]byte("/sessions/${sessionId}/info"), []byte("${window.location.host}/ws/sessions/"),
	} {
		if bytes.Contains(app, legacy) {
			t.Fatalf("dashboard still uses compatibility route %q", legacy)
		}
	}
}

func TestEmbeddedDashboardAuthenticatesWithoutPersistingOrLeakingToken(t *testing.T) {
	app, err := dashboardFS.ReadFile("dashboard/app.js")
	if err != nil {
		t.Fatalf("read embedded dashboard: %v", err)
	}
	for _, required := range [][]byte{
		[]byte("sessionStorage"),
		[]byte("Authorization"),
		[]byte("Bearer "),
		[]byte("agentd.auth."),
	} {
		if !bytes.Contains(app, required) {
			t.Fatalf("dashboard authentication missing %q", required)
		}
	}
	for _, forbidden := range [][]byte{
		[]byte("localStorage"),
		[]byte("access_token="),
		[]byte("auth_token="),
	} {
		if bytes.Contains(app, forbidden) {
			t.Fatalf("dashboard leaks credential through %q", forbidden)
		}
	}
}

func TestEmbeddedDashboardProvidesLiveAgentConsole(t *testing.T) {
	index, err := dashboardFS.ReadFile("dashboard/index.html")
	if err != nil {
		t.Fatalf("read embedded dashboard index: %v", err)
	}
	console, err := dashboardFS.ReadFile("dashboard/console.js")
	if err != nil {
		t.Fatalf("read embedded dashboard console: %v", err)
	}
	app, err := dashboardFS.ReadFile("dashboard/app.js")
	if err != nil {
		t.Fatalf("read embedded dashboard app: %v", err)
	}
	for _, required := range [][]byte{
		[]byte(`data-tab="console"`),
		[]byte(`id="console-form"`),
		[]byte(`id="console-output"`),
		[]byte(`id="dashboard-auth"`),
		[]byte(`conversation.js`),
		[]byte(`console.js`),
	} {
		if !bytes.Contains(index, required) {
			t.Fatalf("dashboard console markup missing %q", required)
		}
	}
	for _, required := range [][]byte{
		[]byte(`/api/v1/sessions`),
		[]byte(`/input`),
		[]byte(`/interrupt`),
		[]byte(`/cancel`),
		[]byte(`structured_output`),
		[]byte(`execution_policy`),
		[]byte(`agentd.auth.`),
		[]byte(`content.delta`),
		[]byte(`kind, text`),
		[]byte(`effortsByModel`),
		[]byte(`resume_session`),
		[]byte(`Send follow-up`),
		[]byte(`tool-event-details`),
		[]byte(`raw_base64`),
		[]byte(`after_sequence=0&limit=20`),
		[]byte(`provider_session_id`),
		[]byte(`session.resumable`),
		[]byte(`request.resume_session = previousSessionID`),
		[]byte(`session.progress`),
		[]byte(`container_lease`),
		[]byte(`Send warm prompt`),
		[]byte(`/resume-state`),
	} {
		if !bytes.Contains(console, required) {
			t.Fatalf("dashboard console behavior missing %q", required)
		}
	}
	for _, forbidden := range [][]byte{
		[]byte("localStorage"),
		[]byte("readAsDataURL"),
		[]byte("Docker history continuation requires AgentD v2.2.5"),
	} {
		if bytes.Contains(console, forbidden) {
			t.Fatalf("dashboard console persists or overexposes credentials through %q", forbidden)
		}
	}
	if bytes.Contains(app, []byte("window.prompt")) {
		t.Fatal("dashboard authentication must not block on window.prompt")
	}
	for _, required := range [][]byte{[]byte(`data-action="resume"`), []byte(`resumeConsoleSession`)} {
		if !bytes.Contains(app, required) {
			t.Fatalf("dashboard history resume behavior missing %q", required)
		}
	}
}

func TestEmbeddedDashboardReplaysConversationForViewAndContinue(t *testing.T) {
	console, err := dashboardFS.ReadFile("dashboard/console.js")
	if err != nil {
		t.Fatalf("read embedded dashboard console: %v", err)
	}
	app, err := dashboardFS.ReadFile("dashboard/app.js")
	if err != nil {
		t.Fatalf("read embedded dashboard app: %v", err)
	}
	conversation, err := dashboardFS.ReadFile("dashboard/conversation.js")
	if err != nil {
		t.Fatalf("read embedded dashboard conversation helpers: %v", err)
	}
	for _, required := range [][]byte{
		[]byte(`function conversationMessagesFromEvents`),
		[]byte(`async function fetchConversationTranscript`),
	} {
		if !bytes.Contains(conversation, required) {
			t.Fatalf("dashboard conversation helper missing %q", required)
		}
	}
	for _, required := range [][]byte{
		[]byte(`renderConversationTranscript(document.getElementById('console-output')`),
		[]byte(`appendConversationMessage('user', text)`),
	} {
		if !bytes.Contains(console, required) {
			t.Fatalf("dashboard continuation transcript behavior missing %q", required)
		}
	}
	for _, required := range [][]byte{
		[]byte(`await fetchConversationTranscript(sessionId)`),
		[]byte(`renderConversationTranscript(document.getElementById('event-log')`),
	} {
		if !bytes.Contains(app, required) {
			t.Fatalf("dashboard view transcript behavior missing %q", required)
		}
	}
	for _, forbidden := range [][]byte{
		[]byte(`Continuing ${session.session_id.slice(0, 8)}`),
		[]byte(`document.getElementById('console-output').textContent = JSON.stringify(result, null, 2)`),
	} {
		if bytes.Contains(console, forbidden) {
			t.Fatalf("dashboard still discards the conversation through %q", forbidden)
		}
	}
}

func TestAPIPrioritizesOverDashboard(t *testing.T) {
	ts, _ := newTestServer(t)

	// API routes should still work (health endpoint), even if dashboard files don't exist
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK for /health, got %d", resp.StatusCode)
	}
}
