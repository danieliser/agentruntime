package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/api"
	"github.com/danieliser/agentruntime/pkg/buildinfo"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type blockingStartupObserver struct{}

func (*blockingStartupObserver) Sync(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestQualifiedBuildUsesExactStampedDockerImages(t *testing.T) {
	identity := buildinfo.Identity{Version: "2.2.0", Commit: "0123456789abcdef0123456789abcdef01234567"}
	cfg := dockerConfigForBuild(t.TempDir(), "ssh://docker.example", identity)
	if cfg.Image != "agentruntime-agent:2.2.0" || cfg.ProxyImage != "agentruntime-proxy:2.2.0" {
		t.Fatalf("qualified Docker image config = %+v", cfg)
	}
	if cfg.ExpectedVersion != identity.Version || cfg.ExpectedCommit != identity.Commit {
		t.Fatalf("qualified Docker stamps = %+v", cfg)
	}
	if cfg.Host != "ssh://docker.example" {
		t.Fatalf("qualified Docker host = %q", cfg.Host)
	}

	dev := dockerConfigForBuild(t.TempDir(), "", buildinfo.Identity{Version: "dev", Commit: "unknown"})
	if dev.Image != runtime.DefaultDockerImage || dev.ProxyImage != "agentruntime-proxy:latest" || dev.ExpectedVersion != "" || dev.ExpectedCommit != "" {
		t.Fatalf("development Docker config = %+v", dev)
	}
}

func TestDockerAgentImageProvidesBubblewrapOnPath(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "docker", "Dockerfile.agent"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dockerfile, []byte("bubblewrap")) {
		t.Fatal("runtime image does not install provider-required bubblewrap")
	}
}

func TestStartupObserverCatchupIsBounded(t *testing.T) {
	started := time.Now()
	err := syncObserversAtStartup(context.Background(), &blockingStartupObserver{}, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup observer sync error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("startup observer sync blocked for %s", elapsed)
	}
}

func TestInstallerLaunchdPathCoversDockerLocations(t *testing.T) {
	installer, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	content := string(installer)
	for _, required := range []string{"<key>EnvironmentVariables</key>", "/usr/local/bin", "/opt/homebrew/bin"} {
		if !strings.Contains(content, required) {
			t.Fatalf("launchd installer is missing %q", required)
		}
	}
	for _, required := range []string{"buildinfo.Version", "buildinfo.Commit", "agentruntime-agent:${AGENTD_VERSION}", "org.opencontainers.image.revision"} {
		if !strings.Contains(content, required) {
			t.Fatalf("installer release qualification is missing %q", required)
		}
	}
}

type recoveryTestHandle struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter
	done    chan runtime.ExitResult
	killed  atomic.Bool
}

func newRecoveryTestHandle() *recoveryTestHandle {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &recoveryTestHandle{
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		stderrR: stderrR,
		stderrW: stderrW,
		done:    make(chan runtime.ExitResult, 1),
	}
}

func (h *recoveryTestHandle) Stdin() io.WriteCloser { return nil }
func (h *recoveryTestHandle) Stdout() io.ReadCloser { return h.stdoutR }
func (h *recoveryTestHandle) Stderr() io.ReadCloser { return h.stderrR }
func (h *recoveryTestHandle) Wait() <-chan runtime.ExitResult {
	return h.done
}
func (h *recoveryTestHandle) Kill() error { h.killed.Store(true); return nil }
func (h *recoveryTestHandle) PID() int    { return 0 }
func (h *recoveryTestHandle) RecoveryInfo() *runtime.RecoveryInfo {
	return nil
}

func TestDaemonRecoveryRejectsUnversionedProcessAndLog(t *testing.T) {
	logDir := t.TempDir()
	handle := newRecoveryTestHandle()
	defer handle.stdoutW.Close()
	defer handle.stderrW.Close()

	sess := &session.Session{
		ID:     "sess-recovered",
		State:  session.StateOrphaned,
		Replay: session.NewReplayBuffer(1024),
		Handle: handle,
	}

	logPath := session.LogFilePath(logDir, sess.ID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("prior output\n"), 0o644); err != nil {
		t.Fatalf("write prior log: %v", err)
	}

	server := api.NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), api.ServerConfig{LogDir: logDir})
	server.RestoreRecoveredSessions([]*session.Session{sess})

	if !handle.killed.Load() {
		t.Fatal("unversioned recovered process was not stopped")
	}
	replayed, next := sess.Replay.ReadFrom(0)
	if len(replayed) != 0 || next != 0 {
		t.Fatalf("unverified legacy bytes were admitted: data=%q next=%d", replayed, next)
	}
}

func TestDefaultDataDirUsesAgentDHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTRUNTIME_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg-must-not-win"))

	if got, want := defaultDataDir(), filepath.Join(home, ".agentd"); got != want {
		t.Fatalf("defaultDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultListenHostIsLiteralLoopback(t *testing.T) {
	if got := defaultListenHost(); got != "127.0.0.1" {
		t.Fatalf("default listen host = %q, want literal loopback", got)
	}
}

func TestDefaultDataDirHonorsExplicitOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "agentd-state")
	t.Setenv("AGENTRUNTIME_DATA_DIR", want)

	if got := defaultDataDir(); got != want {
		t.Fatalf("defaultDataDir() = %q, want explicit override %q", got, want)
	}
}

func TestDefaultPluginConfigUsesDataRootAndExplicitOverride(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTRUNTIME_PLUGIN_CONFIG", "")
	if got, want := defaultPluginConfigPath(dataDir), filepath.Join(dataDir, "plugins.json"); got != want {
		t.Fatalf("defaultPluginConfigPath() = %q, want %q", got, want)
	}
	want := filepath.Join(t.TempDir(), "observers.json")
	t.Setenv("AGENTRUNTIME_PLUGIN_CONFIG", want)
	if got := defaultPluginConfigPath(dataDir); got != want {
		t.Fatalf("defaultPluginConfigPath() = %q, want override %q", got, want)
	}
}

func TestOpenDurableStoreUsesDataRoot(t *testing.T) {
	root := t.TempDir()
	store, err := openDurableStore(root)
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close durable store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agentd.sqlite")); err != nil {
		t.Fatalf("durable database is not under data root: %v", err)
	}
}

func TestConfiguredDiagnosticLogsEnvironmentOverridesFlags(t *testing.T) {
	t.Setenv("AGENTD_DIAGNOSTIC_LOGS", "false")
	t.Setenv("AGENTD_DIAGNOSTIC_LOG_RETENTION", "24h")
	config, err := configuredDiagnosticLogs(true, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if config.Enabled || config.Retention != 24*time.Hour {
		t.Fatalf("diagnostic log config = %+v", config)
	}
}

func TestConfiguredDiagnosticLogsRejectsInvalidEnvironment(t *testing.T) {
	for name, value := range map[string]string{
		"AGENTD_DIAGNOSTIC_LOGS":          "sometimes",
		"AGENTD_DIAGNOSTIC_LOG_RETENTION": "forever",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTD_DIAGNOSTIC_LOGS", "")
			t.Setenv("AGENTD_DIAGNOSTIC_LOG_RETENTION", "")
			t.Setenv(name, value)
			if _, err := configuredDiagnosticLogs(true, 7*24*time.Hour); err == nil {
				t.Fatalf("invalid %s=%q was accepted", name, value)
			}
		})
	}
}

func TestLocalRuntimeIsNativeAndLegacyAliasIsRetired(t *testing.T) {
	rt, err := newRuntime("local", t.TempDir(), "", buildinfo.Identity{Version: "dev", Commit: "unknown"})
	if err != nil {
		t.Fatalf("new local runtime: %v", err)
	}
	if _, ok := rt.(*runtime.LocalRuntime); !ok {
		t.Fatalf("local runtime = %T, want native local runtime", rt)
	}
	if _, err := newRuntime("local-pipe", t.TempDir(), "", buildinfo.Identity{Version: "dev", Commit: "unknown"}); err == nil {
		t.Fatal("retired local-pipe alias remains accepted")
	}
}
