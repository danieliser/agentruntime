package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/danieliser/agentruntime/internal/testutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/pelletier/go-toml/v2"
)

// Runs only through the fake binary wrapper. The real provider is never called.
func TestRestrictedLocalFakeProvider(t *testing.T) {
	if os.Getenv("AGENTD_RESTRICTED_FAKE_PROVIDER") == "" {
		return
	}
	for _, arg := range os.Args {
		if strings.Contains(arg, "dangerously") || strings.Contains(arg, "bypassPermissions") || strings.Contains(arg, "sandbox_mode=") || strings.Contains(arg, "danger-full-access") {
			os.Exit(10)
		}
	}
	for _, arg := range []string{"app-server", "--strict-config", `approval_policy="on-request"`, `default_permissions="agentd-local-v1"`} {
		if !slices.Contains(os.Args, arg) {
			os.Exit(11)
		}
	}
	var cfg struct {
		Default     string `toml:"default_permissions"`
		Approval    string `toml:"approval_policy"`
		Permissions map[string]struct {
			Network struct {
				Enabled bool `toml:"enabled"`
			} `toml:"network"`
			Filesystem map[string]string `toml:"filesystem"`
		} `toml:"permissions"`
	}
	config, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	if err != nil || toml.Unmarshal(config, &cfg) != nil {
		os.Exit(12)
	}
	profile, exists := cfg.Permissions["agentd-local-v1"]
	for _, path := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		if profile.Filesystem[path] != "deny" {
			os.Exit(16)
		}
	}
	if os.Getenv("TMPDIR") == "" || os.Getenv("TMP") != os.Getenv("TMPDIR") || os.Getenv("TEMP") != os.Getenv("TMPDIR") {
		os.Exit(17)
	}
	projects := map[string]struct {
		Trust string `toml:"trust_level"`
	}{}
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "projects=") {
			var config struct {
				Projects map[string]struct {
					Trust string `toml:"trust_level"`
				} `toml:"projects"`
			}
			if toml.Unmarshal([]byte(arg), &config) != nil {
				os.Exit(18)
			}
			projects = config.Projects
		}
	}
	if len(projects) == 0 {
		os.Exit(19)
	}
	for _, project := range projects {
		if project.Trust != "untrusted" {
			os.Exit(20)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(13)
	}
	// macOS may canonicalize the temporary-directory alias in the subprocess cwd.
	workspaceGranted := false
	for path, access := range profile.Filesystem {
		resolved, _ := filepath.EvalSymlinks(path)
		if resolved == cwd && access == "write" {
			workspaceGranted = true
		}
	}
	if !exists || cfg.Default != "agentd-local-v1" || cfg.Approval != "on-request" || profile.Network.Enabled || !workspaceGranted || profile.Filesystem["/"] != "" || profile.Filesystem[os.Getenv("CODEX_HOME")] != "deny" {
		os.Exit(14)
	}
	sessions, err := filepath.EvalSymlinks(filepath.Join(os.Getenv("CODEX_HOME"), "sessions"))
	if err != nil {
		os.Exit(15)
	}
	stateDir := filepath.Dir(sessions)
	send := func(value any) {
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			os.Exit(2)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			os.Exit(3)
		}
		switch msg.Method {
		case "initialize":
			send(map[string]any{"id": msg.ID, "result": map[string]any{"userAgent": "fake-codex"}})
		case "thread/start", "thread/resume", "turn/start":
			var params struct {
				Cwd         string          `json:"cwd"`
				Permissions string          `json:"permissions"`
				Approval    string          `json:"approvalPolicy"`
				Sandbox     json.RawMessage `json:"sandbox"`
				Legacy      json.RawMessage `json:"sandboxPolicy"`
			}
			if json.Unmarshal(msg.Params, &params) != nil || params.Permissions != "agentd-local-v1" || params.Approval != "on-request" || len(params.Sandbox) > 0 || len(params.Legacy) > 0 {
				os.Exit(4)
			}
			if msg.Method != "turn/start" {
				physical, _ := filepath.EvalSymlinks(params.Cwd)
				if cwd != physical {
					os.Exit(5)
				}
				send(map[string]any{"id": msg.ID, "result": map[string]any{"threadId": "fake-thread", "activePermissionProfile": map[string]any{"id": "agentd-local-v1"}}})
			} else {
				send(map[string]any{"id": msg.ID, "result": map[string]any{"turn": map[string]any{"id": "fake-turn"}}})
				send(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "fake-thread", "turn": map[string]any{"id": "fake-turn"}}})
				send(map[string]any{"id": 42, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "fake-thread", "turnId": "fake-turn", "command": "fixture-action"}})
			}
		}
		if string(msg.ID) == "42" && len(msg.Result) > 0 {
			if os.WriteFile(filepath.Join(stateDir, "approval-reply.json"), msg.Result, 0600) != nil {
				os.Exit(6)
			}
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "fake-thread", "turn": map[string]any{"id": "fake-turn", "status": "completed"}}})
			os.Exit(0)
		}
	}
	os.Exit(7)
}

func TestRestrictedLocalApprovalRoundTrip(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("only macOS is qualified")
	}
	for _, decision := range []string{"accept", "decline"} {
		t.Run(decision, func(t *testing.T) {
			root := t.TempDir()
			fakeDir := filepath.Join(root, "bin")
			if err := os.Mkdir(fakeDir, 0700); err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
			script := testutil.CodexQualificationScript(fmt.Sprintf("export AGENTD_RESTRICTED_FAKE_PROVIDER=codex\nexec %s -test.run=^TestRestrictedLocalFakeProvider$ -- \"$@\"", quote(binary)))
			if err := os.WriteFile(filepath.Join(fakeDir, "codex"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			runtime.InitializeRestrictedLocalQualification(t.TempDir())
			awaitLocalQualification(t)
			t.Setenv("HOME", filepath.Join(root, "ambient-home"))
			t.Setenv("CODEX_HOME", filepath.Join(root, "ambient-codex"))
			store, err := durablesqlite.Open(filepath.Join(root, "agentd.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			manager := session.NewManager()
			t.Cleanup(manager.ShutdownAll)
			server := NewServer(manager, runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DataDir: root, LogDir: filepath.Join(root, "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
			httpServer := httptest.NewServer(server.router)
			t.Cleanup(httpServer.Close)
			workDir := filepath.Join(t.TempDir(), "my.app")
			if err := os.Mkdir(workDir, 0700); err != nil {
				t.Fatal(err)
			}
			request := SessionRequest{IdempotencyKey: "fake-local-" + decision, Agent: "codex", Runtime: "local", WorkDir: workDir, Prompt: "request approval", Timeout: "15s", ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
			response := postV1Session(t, httpServer.URL, request)
			var created struct {
				Data v1SessionData `json:"data"`
			}
			decodeJSON(t, response.Body, &created)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusCreated || created.Data.SessionID == "" {
				t.Fatalf("create status=%d", response.StatusCode)
			}
			id := created.Data.SessionID
			var pending []nativeprotocol.PendingApproval
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				res, err := http.Get(httpServer.URL + "/api/v1/sessions/" + id + "/approvals")
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					Data struct {
						Approvals []nativeprotocol.PendingApproval `json:"approvals"`
					} `json:"data"`
				}
				decodeJSON(t, res.Body, &body)
				_ = res.Body.Close()
				pending = body.Data.Approvals
				if len(pending) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(pending) != 1 {
				t.Fatal("approval missing; argv/config/native-policy assertion may have failed")
			}
			if !strings.Contains(pending[0].Warning, "runs outside the sandbox on the host") {
				t.Fatal("host escalation warning missing from pending approval")
			}
			payload, _ := json.Marshal(v1ApprovalDecision{RequestID: pending[0].RequestID, Decision: decision, IdempotencyKey: "fake-decision"})
			res, err := http.Post(httpServer.URL+"/api/v1/sessions/"+id+"/approvals", "application/json", strings.NewReader(string(payload)))
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusAccepted {
				t.Fatalf("approval status=%d", res.StatusCode)
			}
			waitForDurableTerminal(t, store, id)
			stored, err := store.GetSession(context.Background(), id)
			if err != nil || stored.State != durable.StateCompleted {
				t.Fatalf("state=%s err=%v", stored.State, err)
			}
			generation, err := store.GetGeneration(context.Background(), id, 1)
			if err != nil || generation.SandboxProfile != "codex-read-confined-v1" {
				t.Fatal("wrong profile", err)
			}
			stateDir := filepath.Join(root, "codex-sessions", id)
			reply, err := os.ReadFile(filepath.Join(stateDir, "approval-reply.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(reply), `"decision":"`+decision+`"`) {
				t.Fatal("wrong provider decision")
			}
			if _, err := os.Stat(filepath.Join(stateDir, "local-home")); !os.IsNotExist(err) {
				t.Fatal("credential-bearing launch home remains")
			}
			events, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: id, Limit: 1000})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events.Events {
				if event.Type == "control.approval.request" {
					if !strings.Contains(string(event.Payload), "runs outside the sandbox on the host") {
						t.Fatal("host escalation warning missing from durable event")
					}
					found = true
				}
			}
			if !found {
				t.Fatal("approval event missing")
			}
		})
	}
}
