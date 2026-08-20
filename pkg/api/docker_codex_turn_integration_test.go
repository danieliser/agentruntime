package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestDockerRestrictedCodexCompleteTurnQualification(t *testing.T) {
	authJSON := codexQualificationAuth(t)
	testCases := []struct {
		name          string
		authJSON      []byte
		hosts         []string
		wantState     durable.SessionState
		wantErrorCode string
		wantHost      string
	}{
		{
			name: "complete turn", authJSON: authJSON,
			hosts:     []string{"api.openai.com", "auth.openai.com", "chatgpt.com"},
			wantState: durable.StateCompleted,
		},
		{
			name: "missing auth endpoint is attributed", authJSON: nearExpiryCodexAuth(t, authJSON),
			hosts:     []string{"api.openai.com", "chatgpt.com"},
			wantState: durable.StateFailed, wantErrorCode: string(durable.CodeEgressDenied), wantHost: "auth.openai.com",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			runDockerCodexTurnQualification(t, testCase.authJSON, testCase.hosts, testCase.wantState, testCase.wantErrorCode, testCase.wantHost)
		})
	}
}

func codexQualificationAuth(t *testing.T) []byte {
	t.Helper()
	if os.Getenv("AGENTRUNTIME_CODEX_TURN_INTEGRATION") != "1" {
		t.Skip("set AGENTRUNTIME_CODEX_TURN_INTEGRATION=1 to run the paid Codex turn qualification")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	for _, image := range []string{"agentruntime-agent:2.2.4", "agentruntime-proxy:2.2.4"} {
		if output, err := exec.Command("docker", "image", "inspect", image).CombinedOutput(); err != nil {
			t.Skipf("required image %q is unavailable: %v: %s", image, err, output)
		}
	}
	authPath := os.Getenv("AGENTRUNTIME_CODEX_AUTH_FILE")
	if authPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("resolve Codex auth path: %v", err)
		}
		authPath = filepath.Join(home, ".codex", "auth.json")
	}
	authJSON, err := os.ReadFile(authPath)
	if err != nil {
		t.Skipf("read explicitly selected Codex auth file: %v", err)
	}
	var authObject map[string]any
	if json.Unmarshal(authJSON, &authObject) != nil || len(authObject) == 0 {
		t.Skip("explicitly selected Codex auth file is not a nonempty JSON object")
	}
	return authJSON
}

func nearExpiryCodexAuth(t *testing.T, source []byte) []byte {
	t.Helper()
	var auth map[string]any
	if err := json.Unmarshal(source, &auth); err != nil {
		t.Fatal(err)
	}
	tokens, ok := auth["tokens"].(map[string]any)
	if !ok {
		t.Skip("Codex qualification auth has no refreshable token object")
	}
	accessToken, ok := tokens["access_token"].(string)
	parts := strings.Split(accessToken, ".")
	if !ok || len(parts) != 3 {
		t.Skip("Codex qualification access token is not a JWT")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Skipf("decode Codex qualification JWT metadata: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Skipf("decode Codex qualification JWT claims: %v", err)
	}
	payload["exp"] = time.Now().Add(2 * time.Minute).Unix()
	payloadBytes, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(payloadBytes)
	tokens["access_token"] = strings.Join(parts, ".")
	encoded, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func runDockerCodexTurnQualification(t *testing.T, authJSON []byte, hosts []string, wantState durable.SessionState, wantErrorCode, wantHost string) {
	t.Helper()
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	store, err := durablesqlite.Open(filepath.Join(root, "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dockerRuntime := runtime.NewDockerRuntime(runtime.DockerConfig{
		Image: "agentruntime-agent:2.2.4", ProxyImage: "agentruntime-proxy:2.2.4",
		DataDir: root, DiagnosticDir: logDir,
	})
	qualificationRuntime := &retainedQualificationRuntime{DockerRuntime: dockerRuntime}
	manager := session.NewManager()
	t.Cleanup(manager.ShutdownAll)
	server := NewServer(manager, qualificationRuntime, agent.DefaultRegistry(), ServerConfig{
		DataDir: root, LogDir: logDir, DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)

	request := SessionRequest{
		IdempotencyKey: "v224-restricted-codex-turn-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Agent:          "codex", Runtime: "docker", Model: "gpt-5.6-sol", Effort: "high",
		Prompt: `Return only {"ok":true}.`, Timeout: "90s", Context: "clean", AutoDiscover: false,
		Env: map[string]string{CodexAuthJSONEnv: string(authJSON)}, SecretGrants: []string{CodexAuthJSONEnv},
		ExecutionPolicy: &ExecutionPolicy{
			Version: ExecutionPolicyVersion, Workspace: "ephemeral", WorkspaceRetention: "terminal_receipt",
			Filesystem: "read_only", Network: "public_https", AllowedTools: []string{"web_search"},
			EgressAllowlist: hosts, EgressDiagnostics: true, MCPServers: []string{}, HostMounts: []string{}, ApprovalPolicy: "never",
			Resources: &ResourceLimits{MemoryBytes: 2 << 30, CPUCores: 2, PIDs: 256, OpenFiles: 1024},
		},
		StructuredOutput: &StructuredOutput{JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"ok":{"const":true,"type":"boolean"}},"required":["ok"]}`)},
	}
	response := postV1Session(t, httpServer.URL, request)
	defer response.Body.Close()
	var created struct {
		Data  v1SessionData    `json:"data"`
		Error apiErrorEnvelope `json:"error"`
	}
	decodeJSON(t, response.Body, &created)
	if created.Data.SessionID == "" {
		t.Fatalf("Codex qualification returned no durable session: status=%d error=%+v", response.StatusCode, created.Error)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = dockerRuntime.ReleaseSession(cleanupCtx, created.Data.SessionID)
	})
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		stored, lookupErr := store.GetSession(context.Background(), created.Data.SessionID)
		if lookupErr == nil && stored.State.Terminal() {
			containerState, _ := exec.Command("docker", "inspect", "--format", "{{json .State}}", "agentruntime-"+strings.Split(created.Data.SessionID, "-")[0]).CombinedOutput()
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
			releaseErr := dockerRuntime.ReleaseSession(releaseCtx, created.Data.SessionID)
			releaseCancel()
			if releaseErr != nil {
				t.Fatalf("release qualification session: %v", releaseErr)
			}
			diagnostics, _ := os.ReadFile(filepath.Join(logDir, created.Data.SessionID+".egress.ndjson"))
			page, _ := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 1000})
			evidence := make([]string, 0)
			for _, event := range page.Events {
				if event.Stream == durable.StreamTerminal {
					evidence = append(evidence, event.Type+":"+string(event.Payload))
				}
			}
			if stored.State != wantState {
				t.Fatalf("restricted Codex turn state=%s want=%s events=%s container=%s CONNECT diagnostics=%s", stored.State, wantState, strings.Join(evidence, "; "), strings.TrimSpace(string(containerState)), strings.TrimSpace(string(diagnostics)))
			}
			if wantErrorCode != "" {
				combined := strings.Join(evidence, "; ") + "\n" + string(diagnostics)
				if !strings.Contains(combined, wantErrorCode) || !strings.Contains(combined, wantHost) || strings.Contains(combined, "context deadline exceeded") {
					t.Fatalf("negative qualification not attributed: events=%s container=%s CONNECT diagnostics=%s", strings.Join(evidence, "; "), strings.TrimSpace(string(containerState)), strings.TrimSpace(string(diagnostics)))
				}
				return
			}
			receipt, receiptErr := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
			if receiptErr != nil || receipt.ArtifactHash == "" || receipt.OutputHash == "" {
				t.Fatalf("complete turn receipt=%+v err=%v", receipt, receiptErr)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("restricted Codex turn did not reach terminal state %s", wantState))
}

// retainedQualificationRuntime keeps terminal containers available until the
// qualification has synchronously captured the diagnostic record.
type retainedQualificationRuntime struct {
	*runtime.DockerRuntime
}

func (*retainedQualificationRuntime) ReleaseSession(context.Context, string) error { return nil }
