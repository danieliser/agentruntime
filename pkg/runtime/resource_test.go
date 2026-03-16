package runtime_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	goruntime "runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bridgepkg "github.com/danieliser/agentruntime/pkg/bridge"
	"github.com/danieliser/agentruntime/pkg/runtime"
	sessionpkg "github.com/danieliser/agentruntime/pkg/session"
	"github.com/gorilla/websocket"
)

const (
	testReplayBufferSize = 1 << 20
)

type bridgeTestHandle struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter
	done    chan runtime.ExitResult
	once    sync.Once
}

func TestRuntimeResourceHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RESOURCE_HELPER_PROCESS") != "1" {
		return
	}

	switch os.Getenv("RESOURCE_HELPER_ACTION") {
	case "write-stdout":
		total, err := strconv.Atoi(os.Getenv("RESOURCE_HELPER_BYTES"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse RESOURCE_HELPER_BYTES: %v", err)
			os.Exit(2)
		}

		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for total > 0 {
			n := len(chunk)
			if total < n {
				n = total
			}
			if _, err := os.Stdout.Write(chunk[:n]); err != nil {
				fmt.Fprintf(os.Stderr, "write stdout: %v", err)
				os.Exit(2)
			}
			total -= n
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown resource helper action %q", os.Getenv("RESOURCE_HELPER_ACTION"))
		os.Exit(2)
	}

	os.Exit(0)
}

func newBridgeTestHandle() *bridgeTestHandle {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &bridgeTestHandle{
		stdinR:  stdinR,
		stdinW:  stdinW,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		stderrR: stderrR,
		stderrW: stderrW,
		done:    make(chan runtime.ExitResult, 1),
	}
}

func (h *bridgeTestHandle) Stdin() io.WriteCloser           { return h.stdinW }
func (h *bridgeTestHandle) Stdout() io.ReadCloser           { return h.stdoutR }
func (h *bridgeTestHandle) Stderr() io.ReadCloser           { return h.stderrR }
func (h *bridgeTestHandle) Wait() <-chan runtime.ExitResult { return h.done }
func (h *bridgeTestHandle) PID() int                        { return 1 }

func (h *bridgeTestHandle) Kill() error {
	h.exit(137)
	return nil
}

func (h *bridgeTestHandle) exit(code int) {
	h.once.Do(func() {
		_ = h.stdinW.Close()
		_ = h.stdoutW.Close()
		_ = h.stderrW.Close()
		h.done <- runtime.ExitResult{Code: code}
		close(h.done)
	})
}

func startBridgeServer(t *testing.T, handle runtime.ProcessHandle, replay *sessionpkg.ReplayBuffer, sinceOffset int64) (*websocket.Conn, <-chan struct{}, func()) {
	t.Helper()

	bridgeDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b := bridgepkg.New(conn, handle, replay)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		b.Run(ctx, "resource-session", sinceOffset)
		close(bridgeDone)
	}))

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial websocket: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		server.Close()
	}

	return conn, bridgeDone, cleanup
}

func readFrame(t *testing.T, conn *websocket.Conn) bridgepkg.ServerFrame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frame bridgepkg.ServerFrame
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame
}

func requireCommands(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("skip: %s not available: %v", name, err)
		}
	}
}

func resourceHelperConfig(action string, env map[string]string) runtime.SpawnConfig {
	mergedEnv := map[string]string{
		"GO_WANT_RESOURCE_HELPER_PROCESS": "1",
		"RESOURCE_HELPER_ACTION":          action,
	}
	for key, value := range env {
		mergedEnv[key] = value
	}

	return runtime.SpawnConfig{
		Cmd: []string{os.Args[0], "-test.run=^TestRuntimeResourceHelperProcess$"},
		Env: mergedEnv,
	}
}

func currentAllocBytes() uint64 {
	goruntime.GC()
	debug.FreeOSMemory()
	var ms goruntime.MemStats
	goruntime.ReadMemStats(&ms)
	return ms.Alloc
}

func waitForGoroutinesAtMost(t *testing.T, max int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		goruntime.GC()
		if got := goruntime.NumGoroutine(); got <= max {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine count stayed above limit: got %d, want <= %d", goruntime.NumGoroutine(), max)
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := fn()
		if ok {
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("condition not met before timeout: %v", lastErr)
	}
	t.Fatal("condition not met before timeout")
}

func childPIDs(parentPID int) ([]int, error) {
	cmd := exec.Command("ps", "-axo", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	pids := make([]int, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, err
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		if ppid == parentPID {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func processExists(pid int) (bool, error) {
	cmd := exec.Command("ps", "-o", "pid=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func TestLocalRuntime_ResourceBoundaries_50ProcessesComplete(t *testing.T) {
	requireCommands(t, "sh")

	rt := runtime.NewLocalRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const count = 50
	handles := make([]runtime.ProcessHandle, 0, count)
	for i := 0; i < count; i++ {
		handle, err := rt.Spawn(ctx, runtime.SpawnConfig{
			Cmd: []string{"sh", "-c", fmt.Sprintf("printf 'worker-%d\\n'", i)},
		})
		if err != nil {
			t.Fatalf("spawn %d failed: %v", i, err)
		}
		handles = append(handles, handle)
	}

	errCh := make(chan error, count)
	var wg sync.WaitGroup
	for i, handle := range handles {
		i := i
		handle := handle
		wg.Add(1)
		go func() {
			defer wg.Done()

			var drain sync.WaitGroup
			for _, rc := range []io.ReadCloser{handle.Stdout(), handle.Stderr()} {
				if rc == nil {
					continue
				}
				drain.Add(1)
				go func(r io.ReadCloser) {
					defer drain.Done()
					_, _ = io.Copy(io.Discard, r)
					_ = r.Close()
				}(rc)
			}

			result := <-handle.Wait()
			drain.Wait()
			if result.Err != nil {
				errCh <- fmt.Errorf("worker %d wait error: %w", i, result.Err)
				return
			}
			if result.Code != 0 {
				errCh <- fmt.Errorf("worker %d exit code = %d", i, result.Code)
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

func TestBridge_ResourceBoundaries_10MBStdoutReplayRemainsBounded(t *testing.T) {
	rt := runtime.NewLocalRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const totalBytes = 10 << 20
	handle, err := rt.Spawn(ctx, resourceHelperConfig("write-stdout", map[string]string{
		"RESOURCE_HELPER_BYTES": strconv.Itoa(totalBytes),
	}))
	if err != nil {
		t.Fatalf("spawn large stdout helper: %v", err)
	}

	replay := sessionpkg.NewReplayBuffer(testReplayBufferSize)
	conn, bridgeDone, cleanup := startBridgeServer(t, handle, replay, -1)
	defer cleanup()

	if frame := readFrame(t, conn); frame.Type != "connected" {
		t.Fatalf("expected connected frame, got %q", frame.Type)
	}

	var streamed int
	for {
		frame := readFrame(t, conn)
		switch frame.Type {
		case "stdout":
			streamed += len(frame.Data)
		case "exit":
			if frame.ExitCode == nil || *frame.ExitCode != 0 {
				t.Fatalf("unexpected exit frame: %+v", frame)
			}
			goto done
		}
	}

done:
	select {
	case <-bridgeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not finish after large stdout stream")
	}

	if streamed != totalBytes {
		t.Fatalf("expected %d streamed bytes, got %d", totalBytes, streamed)
	}

	data, nextOffset := replay.ReadFrom(0)
	if nextOffset != totalBytes {
		t.Fatalf("expected replay offset %d, got %d", totalBytes, nextOffset)
	}
	if len(data) != testReplayBufferSize {
		t.Fatalf("expected replay buffer to retain %d bytes, got %d", testReplayBufferSize, len(data))
	}
}

func TestBridge_ResourceBoundaries_InfiniteOutputCleansUpAfterKill(t *testing.T) {
	requireCommands(t, "yes")

	rt := runtime.NewLocalRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	handle, err := rt.Spawn(ctx, runtime.SpawnConfig{Cmd: []string{"yes"}})
	if err != nil {
		t.Fatalf("spawn yes: %v", err)
	}

	replay := sessionpkg.NewReplayBuffer(testReplayBufferSize)
	conn, bridgeDone, cleanup := startBridgeServer(t, handle, replay, -1)
	defer cleanup()

	if frame := readFrame(t, conn); frame.Type != "connected" {
		t.Fatalf("expected connected frame, got %q", frame.Type)
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			var frame bridgepkg.ServerFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type == "exit" {
				return
			}
		}
	}()

	time.Sleep(1 * time.Second)
	if err := handle.Kill(); err != nil {
		t.Fatalf("kill yes: %v", err)
	}

	select {
	case <-bridgeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not clean up after killing infinite-output process")
	}

	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket reader did not exit after bridge cleanup")
	}
}

func TestLocalRuntime_ResourceBoundaries_KillReapsProcessGroup(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("process groups are only asserted on unix-like platforms")
	}

	requireCommands(t, "sh", "sleep", "ps")

	rt := runtime.NewLocalRuntime()
	handle, err := rt.Spawn(context.Background(), runtime.SpawnConfig{
		Cmd: []string{"sh", "-c", "sleep 60 & sleep 60"},
	})
	if err != nil {
		t.Fatalf("spawn forked shell: %v", err)
	}

	parentPID := handle.PID()
	if parentPID <= 0 {
		t.Fatalf("expected positive parent PID, got %d", parentPID)
	}

	var children []int
	waitForCondition(t, 3*time.Second, func() (bool, error) {
		pids, err := childPIDs(parentPID)
		if err != nil {
			return false, err
		}
		children = pids
		return len(children) >= 2, nil
	})

	if err := handle.Kill(); err != nil {
		t.Fatalf("kill parent: %v", err)
	}

	select {
	case result := <-handle.Wait():
		if result.Err != nil {
			t.Fatalf("wait returned error: %v", result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parent process did not exit after kill")
	}

	for _, pid := range children {
		pid := pid
		waitForCondition(t, 5*time.Second, func() (bool, error) {
			exists, err := processExists(pid)
			return !exists, err
		})
	}
}

func TestManager_ResourceBoundaries_1000SessionsStayBounded(t *testing.T) {
	before := currentAllocBytes()

	manager := sessionpkg.NewManager()
	for i := 0; i < 1000; i++ {
		sess := sessionpkg.NewSession(fmt.Sprintf("task-%d", i), "claude", "local")
		if err := manager.Add(sess); err != nil {
			t.Fatalf("add session %d: %v", i, err)
		}
	}

	all := manager.List()
	if len(all) != 1000 {
		t.Fatalf("expected 1000 sessions, got %d", len(all))
	}

	after := currentAllocBytes()
	const maxGrowth = 128 << 20
	var delta uint64
	if after > before {
		delta = after - before
	}
	if delta > maxGrowth {
		t.Fatalf("expected memory growth <= %d bytes, got %d", maxGrowth, delta)
	}
}

func TestReplayBuffer_ResourceBoundaries_100MBWriteStaysCapped(t *testing.T) {
	replay := sessionpkg.NewReplayBuffer(testReplayBufferSize)
	chunk := bytes.Repeat([]byte("z"), testReplayBufferSize)

	const writes = 100
	for i := 0; i < writes; i++ {
		n, err := replay.Write(chunk)
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		if n != len(chunk) {
			t.Fatalf("write %d short count: got %d want %d", i, n, len(chunk))
		}
	}

	const totalBytes = int64(writes * testReplayBufferSize)
	if got := replay.TotalBytes(); got != totalBytes {
		t.Fatalf("expected total bytes %d, got %d", totalBytes, got)
	}

	data, nextOffset := replay.ReadFrom(0)
	if nextOffset != totalBytes {
		t.Fatalf("expected next offset %d, got %d", totalBytes, nextOffset)
	}
	if len(data) != testReplayBufferSize {
		t.Fatalf("expected replay buffer length %d, got %d", testReplayBufferSize, len(data))
	}
	if !bytes.Equal(data, chunk) {
		t.Fatal("expected final replay window to match the last written chunk")
	}
}

func TestBridge_ResourceBoundaries_1000RapidStdinFramesDoNotLeakGoroutines(t *testing.T) {
	before := goruntime.NumGoroutine()

	handle := newBridgeTestHandle()
	go func() {
		_, _ = io.Copy(io.Discard, handle.stdinR)
	}()

	replay := sessionpkg.NewReplayBuffer(testReplayBufferSize)
	conn, bridgeDone, cleanup := startBridgeServer(t, handle, replay, -1)

	if frame := readFrame(t, conn); frame.Type != "connected" {
		cleanup()
		t.Fatalf("expected connected frame, got %q", frame.Type)
	}

	for i := 0; i < 1000; i++ {
		if err := conn.WriteJSON(bridgepkg.ClientFrame{
			Type: "stdin",
			Data: fmt.Sprintf("frame-%04d\n", i),
		}); err != nil {
			cleanup()
			t.Fatalf("write stdin frame %d: %v", i, err)
		}
	}

	_ = conn.Close()
	handle.exit(0)

	select {
	case <-bridgeDone:
	case <-time.After(5 * time.Second):
		cleanup()
		t.Fatal("bridge did not shut down after stdin burst")
	}

	cleanup()
	waitForGoroutinesAtMost(t, before+4, 5*time.Second)
}

func TestManager_ResourceBoundaries_AddRemove1000DoesNotLeakGoroutines(t *testing.T) {
	before := goruntime.NumGoroutine()

	manager := sessionpkg.NewManager()
	sessions := make([]*sessionpkg.Session, 0, 1000)
	for i := 0; i < 1000; i++ {
		sess := sessionpkg.NewSession(fmt.Sprintf("task-%d", i), "codex", "local")
		if err := manager.Add(sess); err != nil {
			t.Fatalf("add session %d: %v", i, err)
		}
		sessions = append(sessions, sess)
	}

	for _, sess := range sessions {
		manager.Remove(sess.ID)
	}

	if got := len(manager.List()); got != 0 {
		t.Fatalf("expected manager to be empty, got %d sessions", got)
	}

	waitForGoroutinesAtMost(t, before+1, 3*time.Second)
}
