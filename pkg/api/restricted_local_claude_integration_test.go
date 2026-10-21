package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/internal/testutil"
	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/materialize"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// The child is this Go test executable, never the installed Claude CLI.
func TestRestrictedLocalClaudeFakeProvider(t *testing.T) {
	if os.Getenv("AGENTD_RESTRICTED_FAKE_PROVIDER") != "claude" {
		return
	}
	fail := func(code int) { os.Exit(code) }
	for _, arg := range os.Args {
		for _, bad := range []string{"dangerously", "bypassPermissions", "--allowedTools", "--add-dir", "--ide"} {
			if strings.Contains(arg, bad) {
				fail(10)
			}
		}
	}
	for _, arg := range []string{"--restricted", "--print", "--permission-prompt-tool", "stdio", "--permission-prompts", "host", "--strict-mcp-config", "Read,Glob,Grep,Edit,Write"} {
		if !slices.Contains(os.Args, arg) {
			fail(11)
		}
	}
	settingsPath := ""
	for i, arg := range os.Args {
		if arg == "--settings" && i+1 < len(os.Args) {
			settingsPath = os.Args[i+1]
		}
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fail(12)
	}
	var settings materialize.RestrictedClaudeSettings
	if json.Unmarshal(data, &settings) != nil || !settings.DisableAllHooks || !settings.Sandbox.Enabled || !settings.Sandbox.FailIfUnavailable || settings.Sandbox.AllowUnsandboxedCommands || !settings.Permissions.BlockReadsOutsideWorkingDirectories || len(settings.Permissions.Allow) > 0 {
		fail(13)
	}
	for _, tool := range []string{"Edit", "Write", "Bash"} {
		if !slices.Contains(settings.Permissions.Ask, tool) {
			fail(14)
		}
	}
	for _, path := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		if !slices.Contains(settings.Sandbox.Filesystem.DenyRead, path) || !slices.Contains(settings.Sandbox.Filesystem.DenyWrite, path) {
			fail(15)
		}
	}
	if len(settings.Sandbox.Network.AllowedDomains) > 0 || !slices.Contains(settings.Sandbox.Network.DeniedDomains, "*") {
		fail(16)
	}
	home := os.Getenv("HOME")
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config != filepath.Join(home, ".claude") || !strings.HasPrefix(os.Getenv("TMPDIR"), home+string(os.PathSeparator)) || os.Getenv("TMP") != os.Getenv("TMPDIR") || os.Getenv("TEMP") != os.Getenv("TMPDIR") || os.Getenv("NODE_OPTIONS") != "" || os.Getenv("CLAUDE_CODE_FUTURE_SESSION_THING") != "" {
		fail(17)
	}
	projects, err := filepath.EvalSymlinks(filepath.Join(config, "projects"))
	if err != nil {
		fail(18)
	}
	state := filepath.Dir(projects)
	cwd, err := os.Getwd()
	if err != nil {
		fail(19)
	}
	send := func(v any) {
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			fail(20)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				ID       string `json:"request_id"`
				Response struct {
					Behavior     string          `json:"behavior"`
					UpdatedInput json.RawMessage `json:"updatedInput"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			fail(21)
		}
		if msg.Type == "user" {
			send(map[string]any{"type": "control_request", "request_id": "write-1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "input": map[string]string{"file_path": filepath.Join(cwd, "fixture.txt"), "content": "synthetic"}}})
		}
		if msg.Type == "control_response" && msg.Response.ID == "write-1" {
			if msg.Response.Response.Behavior == "allow" && !strings.Contains(string(msg.Response.Response.UpdatedInput), "fixture.txt") {
				fail(22)
			}
			if os.WriteFile(filepath.Join(state, "approval-reply.json"), scanner.Bytes(), 0600) != nil {
				fail(23)
			}
			send(map[string]any{"type": "result", "subtype": "success", "result": "fixture completed", "session_id": "11111111-1111-4111-8111-111111111111"})
			os.Exit(0)
		}
	}
	fail(24)
}

func TestRestrictedLocalClaudeApprovalRoundTrip(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	for _, decision := range []string{"accept", "decline"} {
		t.Run(decision, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
			script := testutil.ClaudeQualificationScript(fmt.Sprintf("export AGENTD_RESTRICTED_FAKE_PROVIDER=claude\nexec %s -test.run=^TestRestrictedLocalClaudeFakeProvider$ -- \"$@\"", quote(executable)))
			for name, content := range map[string]string{"claude": script, "codex": "#!/bin/sh\nexit 1\n"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			runtime.InitializeRestrictedLocalQualification(t.TempDir())
			t.Cleanup(runtime.StopRestrictedLocalQualification)
			deadline := time.Now().Add(15 * time.Second)
			for runtime.RestrictedLocalQualificationForAgent("claude").State == "pending" {
				if time.Now().After(deadline) {
					t.Fatal("qualification timeout")
				}
				time.Sleep(time.Millisecond)
			}
			t.Setenv("HOME", filepath.Join(root, "ambient-home"))
			t.Setenv("NODE_OPTIONS", "ambient-sentinel")
			t.Setenv("CLAUDE_CODE_FUTURE_SESSION_THING", "ambient-sentinel")
			store, err := durablesqlite.Open(filepath.Join(root, "agentd.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			manager := session.NewManager()
			t.Cleanup(manager.ShutdownAll)
			server := NewServer(manager, runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DataDir: root, LogDir: filepath.Join(root, "logs"), DurableStore: store, EventBroker: eventstream.New(store)})
			httpServer := httptest.NewServer(server.router)
			t.Cleanup(httpServer.Close)
			req := SessionRequest{IdempotencyKey: "claude-" + decision, Agent: "claude", Runtime: "local", WorkDir: t.TempDir(), Prompt: "fake provider fixture", Timeout: "15s", ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request", Tools: "file_only"}}
			response := postV1Session(t, httpServer.URL, req)
			var created struct {
				Data  v1SessionData   `json:"data"`
				Error json.RawMessage `json:"error"`
			}
			decodeJSON(t, response.Body, &created)
			response.Body.Close()
			if response.StatusCode != http.StatusCreated {
				t.Fatal("create failed", response.StatusCode, string(created.Error))
			}
			id := created.Data.SessionID
			var pending []nativeprotocol.PendingApproval
			deadline = time.Now().Add(8 * time.Second)
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
				res.Body.Close()
				pending = body.Data.Approvals
				if len(pending) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(pending) != 1 || pending[0].Method != "can_use_tool" {
				t.Fatal("write approval missing: launch argv/settings assertions may have failed")
			}
			payload, _ := json.Marshal(v1ApprovalDecision{RequestID: pending[0].RequestID, Decision: decision, IdempotencyKey: "write-decision"})
			res, err := http.Post(httpServer.URL+"/api/v1/sessions/"+id+"/approvals", "application/json", strings.NewReader(string(payload)))
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusAccepted {
				t.Fatal(res.StatusCode)
			}
			waitForDurableTerminal(t, store, id)
			stored, err := store.GetSession(context.Background(), id)
			if err != nil || stored.State != durable.StateCompleted {
				t.Fatal("not completed", err, stored.State)
			}
			if !strings.Contains(string(stored.RequestManifest), `"tools":"file_only"`) {
				t.Fatal("file-only opt-in missing from stored session")
			}
			generation, err := store.GetGeneration(context.Background(), id, 1)
			if err != nil || generation.SandboxProfile != "claude-file-tools-only-v1" {
				t.Fatal("wrong profile", err)
			}
			state := filepath.Join(root, "claude-sessions", id)
			reply, err := os.ReadFile(filepath.Join(state, "approval-reply.json"))
			if err != nil {
				t.Fatal(err)
			}
			want := "allow"
			if decision == "decline" {
				want = "deny"
			}
			if !strings.Contains(string(reply), `"behavior":"`+want+`"`) {
				t.Fatal("wrong native decision")
			}
			if _, err := os.Stat(filepath.Join(state, "local-home")); !os.IsNotExist(err) {
				t.Fatal("launch home retained")
			}
			events, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: id, Limit: 1000})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events.Events {
				if event.Type == "control.approval.request" {
					found = true
				}
			}
			if !found {
				t.Fatal("durable approval request missing")
			}
		})
	}
}
