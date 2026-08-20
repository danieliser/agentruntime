//go:build e2e && concurrency

package e2e

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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/api"
	agentclient "github.com/danieliser/agentruntime/pkg/client"
)

const concurrencySessionCount = 30

type concurrencyOutcome struct {
	Index        int           `json:"index"`
	SessionID    string        `json:"session_id,omitempty"`
	State        string        `json:"state,omitempty"`
	ReceiptState string        `json:"receipt_state,omitempty"`
	LastSequence int64         `json:"last_sequence,omitempty"`
	Latency      time.Duration `json:"latency_ns"`
	Error        string        `json:"error,omitempty"`
}

type processSample struct {
	At          time.Time `json:"at"`
	RSSKiB      int64     `json:"rss_kib,omitempty"`
	VSZKiB      int64     `json:"vsz_kib,omitempty"`
	OpenFiles   int       `json:"open_files,omitempty"`
	ProcessTree int       `json:"process_tree,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type concurrencyResults struct {
	StartedAt        time.Time            `json:"started_at"`
	EndedAt          time.Time            `json:"ended_at"`
	Requested        int                  `json:"requested"`
	Completed        int                  `json:"completed"`
	LatencyP50       time.Duration        `json:"latency_p50_ns"`
	LatencyP95       time.Duration        `json:"latency_p95_ns"`
	LatencyMaximum   time.Duration        `json:"latency_maximum_ns"`
	MaximumRSSKiB    int64                `json:"maximum_rss_kib"`
	MaximumVSZKiB    int64                `json:"maximum_vsz_kib"`
	MaximumOpenFiles int                  `json:"maximum_open_files"`
	MaximumProcesses int                  `json:"maximum_process_tree"`
	Sessions         []concurrencyOutcome `json:"sessions"`
	Samples          []processSample      `json:"samples"`
}

// TestConcurrency_30Sessions is an opt-in, deterministic process-boundary
// scenario. It dispatches 30 provider-native sessions through AgentD's current
// authenticated v1 API and requires 30 immutable completed receipts. The
// fixture emulates Claude's native stream protocol without paid model calls.
//
// Artifacts are retained in AGENTRUNTIME_CONCURRENCY_ARTIFACT_DIR, or in a
// private OS temporary directory printed by the test when the variable is
// omitted.
//
// Run: go test -tags='e2e concurrency' -timeout=300s ./pkg/e2e -run TestConcurrency_30Sessions -count=1 -v
func TestConcurrency_30Sessions(t *testing.T) {
	repo := concurrencyRepoRoot(t)
	artifactDir := concurrencyArtifactDir(t, repo)
	t.Logf("concurrency artifacts: %s", artifactDir)

	fixtureDir := t.TempDir()
	installClaudeConcurrencyFixture(t, fixtureDir)
	daemonBin := buildConcurrencyDaemon(t, repo)
	dataDir := t.TempDir()
	port := concurrencyFreePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	daemon := exec.Command(daemonBin,
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--runtime", "local", "--data-dir", dataDir,
	)
	daemon.Env = append(os.Environ(), "PATH="+fixtureDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var daemonLogs bytes.Buffer
	daemon.Stdout = &daemonLogs
	daemon.Stderr = &daemonLogs
	if err := daemon.Start(); err != nil {
		t.Fatalf("start AgentD: %v", err)
	}
	var stopOnce sync.Once
	stopDaemon := func() {
		stopOnce.Do(func() {
			if daemon.Process != nil {
				_ = daemon.Process.Signal(syscall.SIGTERM)
				_ = daemon.Wait()
			}
		})
	}
	t.Cleanup(func() {
		stopDaemon()
		writePrivateArtifact(t, filepath.Join(artifactDir, "daemon.log"), daemonLogs.Bytes())
	})

	token := waitForConcurrencyDaemon(t, baseURL, dataDir, 30*time.Second)
	client := agentclient.NewAuthenticated(baseURL, token)
	capabilities, err := client.GetCapabilities(context.Background())
	if err != nil {
		t.Fatalf("read capabilities: %v", err)
	}
	writePrivateJSON(t, filepath.Join(artifactDir, "environment.json"), map[string]any{
		"go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"logical_cpus": runtime.NumCPU(), "agentd_version": capabilities.AgentDVersion,
		"agentd_commit": capabilities.CommitHash, "session_count": concurrencySessionCount,
		"runtime": "local", "provider": "claude-native-fixture", "repository": repo,
	})

	startedAt := time.Now().UTC()
	sampleCtx, cancelSamples := context.WithCancel(context.Background())
	samplesDone := make(chan []processSample, 1)
	go sampleConcurrencyProcess(sampleCtx, daemon.Process.Pid, samplesDone)

	start := make(chan struct{})
	outcomeCh := make(chan concurrencyOutcome, concurrencySessionCount)
	for index := 0; index < concurrencySessionCount; index++ {
		go func(index int) {
			<-start
			began := time.Now()
			outcome := concurrencyOutcome{Index: index}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			session, err := client.DispatchDurable(ctx, api.SessionRequest{
				IdempotencyKey: fmt.Sprintf("concurrency-30-%02d", index),
				Agent:          "claude", Runtime: "local", Prompt: fmt.Sprintf("fixture session %02d", index),
			})
			if err != nil {
				outcome.Error = err.Error()
				outcome.Latency = time.Since(began)
				outcomeCh <- outcome
				return
			}
			outcome.SessionID = session.SessionID
			for {
				current, err := client.GetDurableSession(ctx, session.SessionID)
				if err != nil {
					outcome.Error = err.Error()
					break
				}
				outcome.State = current.State
				outcome.LastSequence = current.LastSequence
				if current.State == "completed" {
					receipt, err := client.GetTerminalReceipt(ctx, session.SessionID)
					if err != nil {
						outcome.Error = err.Error()
					} else {
						outcome.ReceiptState = receipt.State
					}
					break
				}
				if current.State == "failed" || current.State == "crashed" || current.State == "cancelled" || current.State == "timed_out" || current.State == "indeterminate" {
					outcome.Error = "unexpected terminal state: " + current.State
					break
				}
				select {
				case <-ctx.Done():
					outcome.Error = ctx.Err().Error()
					break
				case <-time.After(20 * time.Millisecond):
					continue
				}
				break
			}
			outcome.Latency = time.Since(began)
			outcomeCh <- outcome
		}(index)
	}
	close(start)
	outcomes := make([]concurrencyOutcome, 0, concurrencySessionCount)
	for range concurrencySessionCount {
		outcomes = append(outcomes, <-outcomeCh)
	}
	cancelSamples()
	samples := <-samplesDone
	endedAt := time.Now().UTC()
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Index < outcomes[j].Index })
	results := summarizeConcurrencyResults(startedAt, endedAt, outcomes, samples)
	writePrivateJSON(t, filepath.Join(artifactDir, "results.json"), results)
	stopDaemon()

	t.Logf("completed=%d/%d latency p50=%s p95=%s max=%s max_rss=%dKiB max_fds=%d max_processes=%d",
		results.Completed, results.Requested, results.LatencyP50, results.LatencyP95,
		results.LatencyMaximum, results.MaximumRSSKiB, results.MaximumOpenFiles, results.MaximumProcesses)
	if results.Completed != concurrencySessionCount {
		for _, outcome := range outcomes {
			if outcome.ReceiptState != "completed" || outcome.Error != "" {
				t.Logf("session[%d] id=%s state=%s receipt=%s err=%s", outcome.Index, outcome.SessionID, outcome.State, outcome.ReceiptState, outcome.Error)
			}
		}
		t.Fatalf("completed=%d, want exactly %d", results.Completed, concurrencySessionCount)
	}
}

func summarizeConcurrencyResults(startedAt, endedAt time.Time, outcomes []concurrencyOutcome, samples []processSample) concurrencyResults {
	results := concurrencyResults{StartedAt: startedAt, EndedAt: endedAt, Requested: len(outcomes), Sessions: outcomes, Samples: samples}
	latencies := make([]time.Duration, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Error == "" && outcome.State == "completed" && outcome.ReceiptState == "completed" {
			results.Completed++
		}
		latencies = append(latencies, outcome.Latency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		results.LatencyP50 = latencies[(len(latencies)-1)*50/100]
		results.LatencyP95 = latencies[(len(latencies)-1)*95/100]
		results.LatencyMaximum = latencies[len(latencies)-1]
	}
	for _, sample := range samples {
		if sample.RSSKiB > results.MaximumRSSKiB {
			results.MaximumRSSKiB = sample.RSSKiB
		}
		if sample.VSZKiB > results.MaximumVSZKiB {
			results.MaximumVSZKiB = sample.VSZKiB
		}
		if sample.OpenFiles > results.MaximumOpenFiles {
			results.MaximumOpenFiles = sample.OpenFiles
		}
		if sample.ProcessTree > results.MaximumProcesses {
			results.MaximumProcesses = sample.ProcessTree
		}
	}
	return results
}

func sampleConcurrencyProcess(ctx context.Context, pid int, done chan<- []processSample) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var samples []processSample
	for {
		samples = append(samples, readConcurrencyProcessSample(pid))
		select {
		case <-ctx.Done():
			done <- samples
			return
		case <-ticker.C:
		}
	}
}

func readConcurrencyProcessSample(pid int) processSample {
	sample := processSample{At: time.Now().UTC()}
	output, err := exec.Command("ps", "-o", "rss=,vsz=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	fields := strings.Fields(string(output))
	if len(fields) >= 2 {
		sample.RSSKiB, _ = strconv.ParseInt(fields[0], 10, 64)
		sample.VSZKiB, _ = strconv.ParseInt(fields[1], 10, 64)
	}
	if output, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-Fn").Output(); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "f") {
				sample.OpenFiles++
			}
		}
	}
	if output, err := exec.Command("ps", "-axo", "pid=,ppid=").Output(); err == nil {
		children := make(map[int][]int)
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			child, childErr := strconv.Atoi(fields[0])
			parent, parentErr := strconv.Atoi(fields[1])
			if childErr == nil && parentErr == nil {
				children[parent] = append(children[parent], child)
			}
		}
		queue := []int{pid}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			sample.ProcessTree++
			queue = append(queue, children[current]...)
		}
	}
	return sample
}

func installClaudeConcurrencyFixture(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
set -eu
IFS= read -r prompt
sleep 1
printf '%s\n' '{"type":"system","subtype":"init","session_id":"concurrency-fixture"}'
printf '%s\n' '{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"fixture-complete"}}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"fixture-complete"}'
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatalf("write Claude concurrency fixture: %v", err)
	}
}

func buildConcurrencyDaemon(t *testing.T, repo string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "agentd")
	command := exec.Command("go", "build", "-o", binary, "./cmd/agentd")
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build AgentD: %v\n%s", err, output)
	}
	return binary
}

func waitForConcurrencyDaemon(t *testing.T, baseURL, dataDir string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		token, tokenErr := api.ReadAuthToken(dataDir)
		request, _ := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
		response, healthErr := http.DefaultClient.Do(request)
		if healthErr == nil {
			_ = response.Body.Close()
		}
		if tokenErr == nil && healthErr == nil && response.StatusCode == http.StatusOK {
			return token
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("AgentD did not become ready within %s", timeout)
	return ""
}

func concurrencyArtifactDir(t *testing.T, repo string) string {
	t.Helper()
	dir := os.Getenv("AGENTRUNTIME_CONCURRENCY_ARTIFACT_DIR")
	if dir == "" {
		root := filepath.Join(repo, ".artifacts")
		concurrencyRoot := filepath.Join(root, "concurrency")
		dir = filepath.Join(concurrencyRoot, fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), os.Getpid()))
		for _, privateDir := range []string{root, concurrencyRoot, dir} {
			if err := os.MkdirAll(privateDir, 0o700); err != nil {
				t.Fatalf("create concurrency artifact directory: %v", err)
			}
			if err := os.Chmod(privateDir, 0o700); err != nil {
				t.Fatalf("secure concurrency artifact directory: %v", err)
			}
		}
	}
	if os.Getenv("AGENTRUNTIME_CONCURRENCY_ARTIFACT_DIR") != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create concurrency artifact directory: %v", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("secure concurrency artifact directory: %v", err)
		}
	}
	return dir
}

func writePrivateJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode artifact %s: %v", path, err)
	}
	writePrivateArtifact(t, path, append(raw, '\n'))
}

func writePrivateArtifact(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write artifact %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("secure artifact %s: %v", path, err)
	}
}

func concurrencyRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}

func concurrencyFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
