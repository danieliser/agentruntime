package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/agent"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

const dockerRestartHelperEnv = "AGENTRUNTIME_DOCKER_RESTART_HELPER"

// TestAgentDDockerRestartHelperProcess is the daemon half of the qualification
// harness. The parent kills its entire process group so Docker, not an orphaned
// CLI attachment, remains the sole owner of the provider process.
func TestAgentDDockerRestartHelperProcess(t *testing.T) {
	if os.Getenv(dockerRestartHelperEnv) != "1" {
		t.Skip("helper process")
	}
	dataDir := os.Getenv("AGENTRUNTIME_HELPER_DATA_DIR")
	address := os.Getenv("AGENTRUNTIME_HELPER_ADDRESS")
	provider := os.Getenv("AGENTRUNTIME_HELPER_PROVIDER")
	image := os.Getenv("AGENTRUNTIME_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	store, err := durablesqlite.Open(filepath.Join(dataDir, "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open helper store: %v", err)
	}
	defer store.Close()

	dockerRuntime := runtime.NewDockerRuntime(runtime.DockerConfig{Image: image, DataDir: dataDir})
	manager := session.NewManager()
	handles, err := dockerRuntime.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover Docker runtime: %v", err)
	}
	recovered := manager.Recover(handles, dockerRuntime.Name())
	registry := agent.NewRegistry()
	registry.Register(&dockerProtocolFixtureAgent{provider: provider})
	server := NewServer(manager, dockerRuntime, registry, ServerConfig{
		Version: "docker-restart-fixture", DataDir: dataDir,
		LogDir: filepath.Join(dataDir, "logs"), DurableStore: store,
		EventBroker: eventstream.New(store),
	})
	server.RestoreRecoveredSessions(recovered, dockerRuntime.Name())
	if err := server.Start(address); err != nil && err != http.ErrServerClosed {
		t.Fatalf("serve helper: %v", err)
	}
}

type dockerProtocolFixtureAgent struct{ provider string }

func (fixture *dockerProtocolFixtureAgent) Name() string { return fixture.provider }

func (fixture *dockerProtocolFixtureAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	switch fixture.provider {
	case "claude":
		return []string{"/bin/sh", "-c", claudeDockerRestartFixture}, nil
	case "codex":
		return []string{"/bin/sh", "-c", codexDockerRestartFixture}, nil
	default:
		return nil, fmt.Errorf("unsupported fixture provider %q", fixture.provider)
	}
}

func (*dockerProtocolFixtureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) {
	return nil, false
}

const claudeDockerRestartFixture = `
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-fixture-thread"}'
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf '{"type":"stream_event","session_id":"claude-fixture-thread","event":{"delta":{"type":"text_delta","text":"claude-%s"}}}\n' "$count"
  printf '%s\n' '{"type":"result","subtype":"success","session_id":"claude-fixture-thread"}'
done
`

const codexDockerRestartFixture = `
count=0
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"id":0,"result":{"userAgent":"codex-fixture"}}'
      ;;
    *'"method":"thread/start"'*)
      printf '%s\n' '{"id":1,"result":{"thread":{"id":"codex-fixture-thread"}}}'
      printf '%s\n' '{"method":"thread/started","params":{"threadId":"codex-fixture-thread"}}'
      ;;
    *'"method":"turn/start"'*)
      count=$((count + 1))
      printf '{"method":"turn/started","params":{"threadId":"codex-fixture-thread","turnId":"turn-%s"}}\n' "$count"
      printf '{"method":"item/agentMessage/delta","params":{"threadId":"codex-fixture-thread","turnId":"turn-%s","delta":"codex-%s"}}\n' "$count" "$count"
      printf '{"method":"turn/completed","params":{"threadId":"codex-fixture-thread","turnId":"turn-%s","usage":{}}}\n' "$count"
      ;;
  esac
done
`

func TestAgentDRestartReconstructsActiveNativeDockerSessions(t *testing.T) {
	if os.Getenv("AGENTRUNTIME_DOCKER_INTEGRATION") != "1" {
		t.Skip("set AGENTRUNTIME_DOCKER_INTEGRATION=1 to run Docker qualification tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	image := os.Getenv("AGENTRUNTIME_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	if _, err := dockerTestOutput("image", "inspect", image); err != nil {
		t.Skipf("Docker test image %q is unavailable: %v", image, err)
	}
	if existing, err := dockerTestOutput("ps", "-aq", "--filter", "label=agentruntime.session_id"); err != nil {
		t.Fatalf("list existing AgentD containers: %v", err)
	} else if strings.TrimSpace(existing) != "" {
		t.Skip("existing AgentD containers make destructive restart qualification unsafe")
	}
	cleanupProxy := prepareDockerRestartProxy(t, image)
	defer cleanupProxy()

	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			qualifyDockerDaemonRestart(t, provider, image)
		})
	}
}

func qualifyDockerDaemonRestart(t *testing.T, provider, image string) {
	dataDir := t.TempDir()
	sessionID := uuid.NewString()
	address := availableLoopbackAddress(t)
	baseURL := "http://" + address
	t.Cleanup(func() { removeDockerSessionContainers(t, sessionID) })

	first := startDockerRestartHelper(t, provider, image, dataDir, address)
	waitForHTTPHealth(t, baseURL)
	request := SessionRequest{
		SessionID: sessionID, IdempotencyKey: "restart-" + provider,
		Agent: provider, Runtime: "docker", Prompt: "first", Interactive: true,
		Context: "clean", AutoDiscover: false, Timeout: "2m",
		Container: &ContainerConfig{Image: image},
	}
	response := requestJSON(t, http.MethodPost, baseURL+"/api/v1/sessions", request)
	if response.StatusCode != http.StatusCreated {
		body := readResponse(t, response)
		first.stop(t)
		t.Fatalf("create %s session: status=%d body=%s", provider, response.StatusCode, body)
	}
	_ = readResponse(t, response)
	before := waitForEventText(t, baseURL, sessionID, provider+"-1")
	if len(before) == 0 {
		first.stop(t)
		t.Fatal("initial provider output was not persisted")
	}
	first.stop(t)
	waitForDockerSessionCount(t, sessionID, 1, true)

	second := startDockerRestartHelper(t, provider, image, dataDir, address)
	defer second.stop(t)
	waitForHTTPHealth(t, baseURL)
	waitForRunningGeneration(t, baseURL, sessionID, 1)
	replayed := waitForEventCount(t, baseURL, sessionID, len(before))
	assertStableEventPrefix(t, before, replayed)

	response = requestJSON(t, http.MethodPost, baseURL+"/api/v1/sessions/"+sessionID+"/input", map[string]any{
		"idempotency_key": "after-restart-" + provider, "kind": "prompt", "text": "second",
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("send input after restart: status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	_ = readResponse(t, response)
	after := waitForEventText(t, baseURL, sessionID, provider+"-2")
	assertStableEventPrefix(t, before, after)
	assertContiguousUniqueEvents(t, after)
	waitForDockerSessionCount(t, sessionID, 1, true)

	response = requestJSON(t, http.MethodPost, baseURL+"/api/v1/sessions/"+sessionID+"/terminate", map[string]string{
		"idempotency_key": "qualification-cleanup-" + provider,
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("terminate recovered session: status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	_ = readResponse(t, response)
	waitForTerminalReceipt(t, baseURL, sessionID)
}

type dockerRestartChild struct {
	command *exec.Cmd
	output  bytes.Buffer
	stopped bool
}

func startDockerRestartHelper(t *testing.T, provider, image, dataDir, address string) *dockerRestartChild {
	t.Helper()
	child := &dockerRestartChild{}
	child.command = exec.Command(os.Args[0], "-test.run=^TestAgentDDockerRestartHelperProcess$")
	child.command.Env = append(os.Environ(),
		dockerRestartHelperEnv+"=1",
		"AGENTRUNTIME_HELPER_PROVIDER="+provider,
		"AGENTRUNTIME_HELPER_DATA_DIR="+dataDir,
		"AGENTRUNTIME_HELPER_ADDRESS="+address,
		"AGENTRUNTIME_DOCKER_TEST_IMAGE="+image,
		"HOME="+filepath.Join(dataDir, "fixture-home"),
	)
	child.command.Stdout = &child.output
	child.command.Stderr = &child.output
	child.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.command.Start(); err != nil {
		t.Fatalf("start AgentD helper: %v", err)
	}
	return child
}

func (child *dockerRestartChild) stop(t *testing.T) {
	t.Helper()
	if child == nil || child.stopped {
		return
	}
	child.stopped = true
	if child.command.Process != nil {
		_ = syscall.Kill(-child.command.Process.Pid, syscall.SIGKILL)
	}
	if err := child.command.Wait(); err == nil {
		t.Fatalf("AgentD helper exited without forced termination; output=%s", child.output.String())
	}
	if t.Failed() {
		t.Logf("AgentD helper output:\n%s", child.output.String())
	}
}

func availableLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback address: %v", err)
	}
	return address
}

func waitForHTTPHealth(t *testing.T, baseURL string) {
	t.Helper()
	waitForCondition(t, 20*time.Second, func() bool {
		response, err := http.Get(baseURL + "/health")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, "AgentD helper health")
}

func waitForRunningGeneration(t *testing.T, baseURL, sessionID string, generation int64) {
	t.Helper()
	waitForCondition(t, 20*time.Second, func() bool {
		response, err := http.Get(baseURL + "/api/v1/sessions/" + sessionID)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var body struct {
			Data v1SessionData `json:"data"`
		}
		return response.StatusCode == http.StatusOK && json.NewDecoder(response.Body).Decode(&body) == nil &&
			body.Data.State == "running" && body.Data.Generation == generation
	}, "recovered running generation")
}

func waitForEventText(t *testing.T, baseURL, sessionID, marker string) []eventEnvelope {
	t.Helper()
	var events []eventEnvelope
	waitForCondition(t, 20*time.Second, func() bool {
		events = fetchDockerRestartEvents(t, baseURL, sessionID)
		for _, event := range events {
			if strings.Contains(string(event.Payload), marker) {
				return true
			}
		}
		return false
	}, "event payload containing "+marker)
	return events
}

func waitForEventCount(t *testing.T, baseURL, sessionID string, count int) []eventEnvelope {
	t.Helper()
	var events []eventEnvelope
	waitForCondition(t, 20*time.Second, func() bool {
		events = fetchDockerRestartEvents(t, baseURL, sessionID)
		return len(events) >= count
	}, fmt.Sprintf("at least %d replay events", count))
	return events
}

func fetchDockerRestartEvents(t *testing.T, baseURL, sessionID string) []eventEnvelope {
	t.Helper()
	response, err := http.Get(baseURL + "/api/v1/sessions/" + sessionID + "/events?after_sequence=0&limit=1000")
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Data eventPageEnvelope `json:"data"`
	}
	if json.NewDecoder(response.Body).Decode(&body) != nil {
		return nil
	}
	return body.Data.Events
}

func assertStableEventPrefix(t *testing.T, before, after []eventEnvelope) {
	t.Helper()
	if len(after) < len(before) {
		t.Fatalf("replay shrank from %d events to %d", len(before), len(after))
	}
	for index := range before {
		if before[index].EventID != after[index].EventID || before[index].Sequence != after[index].Sequence ||
			before[index].RawSHA256 != after[index].RawSHA256 {
			t.Fatalf("event %d changed across restart: before=%+v after=%+v", index, before[index], after[index])
		}
	}
}

func assertContiguousUniqueEvents(t *testing.T, events []eventEnvelope) {
	t.Helper()
	seen := make(map[string]struct{}, len(events))
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("event sequence[%d]=%d, want %d", index, event.Sequence, index+1)
		}
		if _, duplicate := seen[event.EventID]; duplicate {
			t.Fatalf("duplicate event ID %s", event.EventID)
		}
		seen[event.EventID] = struct{}{}
	}
}

func waitForTerminalReceipt(t *testing.T, baseURL, sessionID string) {
	t.Helper()
	waitForCondition(t, 20*time.Second, func() bool {
		response, err := http.Get(baseURL + "/api/v1/sessions/" + sessionID + "/receipt")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, "terminal receipt")
}

func requestJSON(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	return response
}

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return body.String()
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func prepareDockerRestartProxy(t *testing.T, image string) func() {
	t.Helper()
	createdNetwork := false
	if _, err := dockerTestOutput("network", "inspect", "agentruntime-agents"); err != nil {
		if _, err := dockerTestOutput("network", "create", "agentruntime-agents"); err != nil {
			t.Fatalf("create qualification network: %v", err)
		}
		createdNetwork = true
	}
	if _, err := dockerTestOutput("inspect", "agentruntime-proxy"); err == nil {
		t.Skip("existing agentruntime-proxy makes qualification fixture ownership ambiguous")
	}
	if _, err := dockerTestOutput("run", "-d", "--name", "agentruntime-proxy", "--network", "agentruntime-agents", image, "sleep", "300"); err != nil {
		t.Fatalf("start qualification proxy placeholder: %v", err)
	}
	return func() {
		_, _ = dockerTestOutput("rm", "-f", "agentruntime-proxy")
		if createdNetwork {
			_, _ = dockerTestOutput("network", "rm", "agentruntime-agents")
		}
	}
}

func waitForDockerSessionCount(t *testing.T, sessionID string, count int, running bool) {
	t.Helper()
	waitForCondition(t, 10*time.Second, func() bool {
		args := []string{"ps", "-q"}
		if !running {
			args = []string{"ps", "-aq"}
		}
		args = append(args, "--filter", "label=agentruntime.session_id="+sessionID)
		output, err := dockerTestOutput(args...)
		if err != nil {
			return false
		}
		lines := strings.Fields(output)
		return len(lines) == count
	}, fmt.Sprintf("%d Docker containers for session %s", count, sessionID))
}

func removeDockerSessionContainers(t *testing.T, sessionID string) {
	t.Helper()
	output, err := dockerTestOutput("ps", "-aq", "--filter", "label=agentruntime.session_id="+sessionID)
	if err != nil {
		t.Logf("list cleanup containers: %v", err)
		return
	}
	for _, containerID := range strings.Fields(output) {
		if _, err := dockerTestOutput("rm", "-f", containerID); err != nil {
			t.Logf("remove cleanup container %s: %v", containerID, err)
		}
	}
}

func dockerTestOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
