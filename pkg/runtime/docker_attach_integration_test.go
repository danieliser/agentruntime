package runtime

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDockerDirectAttach_ReconnectsStdinAndRecoversOrderedOutput qualifies the
// direct-attach assumption in STR-002/STR-003. It is opt-in because it requires
// a real Docker daemon and a locally available image.
func TestDockerDirectAttach_ReconnectsStdinAndRecoversOrderedOutput(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := dockerProbe(ctx, "image", "inspect", image); err != nil {
		t.Skipf("Docker test image %q is unavailable locally: %v", image, err)
	}

	name := "agentruntime-g0-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	containerID, err := dockerProbeOutput(ctx,
		"run", "-d", "-i",
		"--name", name,
		"--label", "agentruntime.g0_probe=true",
		"--log-driver", "json-file",
		image,
		"sh", "-c",
		`printf '{"stream":"stderr","value":"boot"}\n' >&2; while IFS= read -r line; do printf '%s\n' "$line"; done`,
	)
	if err != nil {
		t.Fatalf("start probe container: %v", err)
	}
	if strings.TrimSpace(containerID) == "" {
		t.Fatal("start probe container returned no ID")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = dockerProbe(cleanupCtx, "rm", "-f", name)
	})

	first := `{"stream":"provider_stdout","sequence":1}`
	second := `{"stream":"provider_stdout","sequence":2}`
	dockerAttachRoundTrip(t, ctx, name, first)

	state, err := dockerProbeOutput(ctx, "inspect", "--format", "{{.State.Running}}|{{.Config.OpenStdin}}|{{.Config.StdinOnce}}", name)
	if err != nil {
		t.Fatalf("inspect probe container: %v", err)
	}
	if got := strings.TrimSpace(state); got != "true|true|false" {
		t.Fatalf("container cannot accept a later attach: got %q, want true|true|false", got)
	}

	dockerAttachRoundTrip(t, ctx, name, second)

	stdout, stderr, err := dockerProbeLogs(ctx, name)
	if err != nil {
		t.Fatalf("read retained Docker logs: %v", err)
	}
	assertLinesInOrder(t, stdout, first, second)
	if !strings.Contains(stderr, `{"stream":"stderr","value":"boot"}`) {
		t.Fatalf("stderr log was not retained separately: %q", stderr)
	}
	assertTimestampedLog(t, stdout)
	assertTimestampedLog(t, stderr)
}

func dockerAttachRoundTrip(t *testing.T, parent context.Context, containerName, record string) {
	t.Helper()

	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, "docker", "attach", "--sig-proxy=false", containerName)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("attach stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("attach stdout: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start docker attach: %v", err)
	}

	if _, err := io.WriteString(stdin, record+"\n"); err != nil {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("write attached stdin: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == record {
				result <- nil
				return
			}
		}
		if err := scanner.Err(); err != nil {
			result <- err
			return
		}
		result <- io.EOF
	}()

	select {
	case err := <-result:
		if err != nil {
			cancel()
			_ = cmd.Wait()
			t.Fatalf("read attached stdout: %v (stderr: %s)", err, strings.TrimSpace(stderr.String()))
		}
	case <-time.After(5 * time.Second):
		cancel()
		_ = cmd.Wait()
		t.Fatalf("timed out waiting for attached output %s (stderr: %s)", record, strings.TrimSpace(stderr.String()))
	}

	cancel()
	_ = stdin.Close()
	_ = cmd.Wait()
}

func dockerProbe(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func dockerProbeOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func dockerProbeLogs(ctx context.Context, containerName string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "--timestamps", containerName)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("docker logs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}

func assertLinesInOrder(t *testing.T, output string, records ...string) {
	t.Helper()

	position := 0
	for _, record := range records {
		recordPosition := strings.Index(output[position:], record)
		if recordPosition < 0 {
			t.Fatalf("retained log missing record %s after byte %d: %q", record, position, output)
		}
		position += recordPosition + len(record)
	}
}

func assertTimestampedLog(t *testing.T, output string) {
	t.Helper()

	line, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	timestamp, _, ok := strings.Cut(line, " ")
	if !ok {
		t.Fatalf("Docker log line has no timestamp separator: %q", line)
	}
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		t.Fatalf("Docker log timestamp %q is not RFC3339Nano: %v", timestamp, err)
	}
}
