package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestLoadOrCreateAuthTokenIsPrivateStableAndUnlogged(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "agentd")
	first, err := LoadOrCreateAuthToken(dataDir)
	if err != nil {
		t.Fatalf("create auth token: %v", err)
	}
	second, err := LoadOrCreateAuthToken(dataDir)
	if err != nil {
		t.Fatalf("reload auth token: %v", err)
	}
	if first == "" || first != second || len(first) < 40 {
		t.Fatalf("unstable or weak token: first length=%d equal=%v", len(first), first == second)
	}
	for _, target := range []struct {
		path string
		mode os.FileMode
	}{{dataDir, 0o700}, {filepath.Join(dataDir, AuthTokenFilename), 0o600}} {
		info, err := os.Stat(target.path)
		if err != nil {
			t.Fatalf("stat %s: %v", target.path, err)
		}
		if got := info.Mode().Perm(); got != target.mode {
			t.Errorf("mode %s = %#o, want %#o", target.path, got, target.mode)
		}
	}
}

func TestLoadOrCreateAuthTokenIsConcurrentSafe(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "agentd")
	const callers = 16
	tokens := make(chan string, callers)
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			token, err := LoadOrCreateAuthToken(dataDir)
			if err != nil {
				errors <- err
				return
			}
			tokens <- token
		}()
	}
	group.Wait()
	close(tokens)
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent token creation: %v", err)
	}
	var want string
	for token := range tokens {
		if want == "" {
			want = token
		}
		if token != want {
			t.Fatalf("concurrent callers received different credentials")
		}
	}
}

func TestLoadOrCreateAuthTokenRejectsUnsafeExistingFile(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, dataDir, tokenPath string)
	}{
		{
			name: "broad permissions",
			setup: func(t *testing.T, _, tokenPath string) {
				t.Helper()
				if err := os.WriteFile(tokenPath, []byte(strings.Repeat("a", 43)), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, dataDir, tokenPath string) {
				t.Helper()
				target := filepath.Join(dataDir, "elsewhere")
				if err := os.WriteFile(target, []byte(strings.Repeat("b", 43)), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, tokenPath); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "agentd")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, dataDir, filepath.Join(dataDir, AuthTokenFilename))
			if _, err := LoadOrCreateAuthToken(dataDir); err == nil {
				t.Fatal("expected unsafe credential file to be rejected")
			}
		})
	}
}

func TestAuthenticatedServerProtectsPrivateSurfaces(t *testing.T) {
	const token = "test-token-that-is-long-enough-for-authentication-1234"
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		AuthToken: token, ListenerScope: "loopback",
	})
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)

	assertStatus := func(method, path, bearer string, want int) {
		t.Helper()
		request, err := http.NewRequest(method, httpServer.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("%s %s status=%d, want %d", method, path, response.StatusCode, want)
		}
	}

	assertStatus(http.MethodGet, "/health", "", http.StatusOK)
	for _, path := range []string{
		"/api/v1/capabilities", "/api/v1/sessions", "/api/v1/ws/sessions/missing/events", "/chats",
	} {
		assertStatus(http.MethodGet, path, "", http.StatusUnauthorized)
		assertStatus(http.MethodGet, path, "wrong-token-with-equivalent-public-length-0000", http.StatusUnauthorized)
	}
	assertStatus(http.MethodGet, "/api/v1/capabilities", token, http.StatusOK)
	assertStatus(http.MethodGet, "/api/v1/sessions", token, http.StatusServiceUnavailable)
	assertStatus(http.MethodGet, "/api/v1/ws/sessions/missing/events", token, http.StatusServiceUnavailable)
}

func TestAuthenticatedServerAcceptsBearerWebSocketSubprotocolAtSameOrigin(t *testing.T) {
	const token = "test-token-that-is-long-enough-for-websocket-auth-1234"
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		AuthToken: token, ListenerScope: "loopback",
	})
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/ws/sessions/missing/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Protocol", "agentd.v1, agentd.auth."+token)
	request.Header.Set("Origin", httpServer.URL)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("valid websocket subprotocol credential rejected: %s", body)
	}
}

func TestWebSocketOriginPolicyRejectsCrossOriginBrowser(t *testing.T) {
	for _, test := range []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{name: "non browser", host: "127.0.0.1:8090", want: true},
		{name: "same origin", host: "127.0.0.1:8090", origin: "http://127.0.0.1:8090", want: true},
		{name: "cross origin", host: "127.0.0.1:8090", origin: "https://attacker.example", want: false},
		{name: "invalid scheme", host: "127.0.0.1:8090", origin: "file:///tmp/dashboard.html", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/api/v1/ws/sessions/id/events", nil)
			request.Header.Set("Origin", test.origin)
			if got := sameOriginWebSocket(request); got != test.want {
				t.Fatalf("sameOriginWebSocket() = %v, want %v", got, test.want)
			}
		})
	}
}
