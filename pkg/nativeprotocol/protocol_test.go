package nativeprotocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeTransportReportsStreamReadFailure(t *testing.T) {
	adapter, err := NewAdapter(ProviderClaude)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	want := errors.New("fixture read failure")
	wait := make(chan Exit, 1)
	wait <- Exit{Code: 0}
	close(wait)
	transport := NewStreamTransport(adapter, ProcessIO{
		Stdout: &errorReadCloser{err: want},
		Wait:   wait,
	}, RecoveryMetadata{SessionID: "stream-error", Generation: 1})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("start transport: %v", err)
	}
	select {
	case exit := <-transport.Wait():
		if !errors.Is(exit.Err, want) {
			t.Fatalf("exit error = %v, want %v", exit.Err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transport exit")
	}
}

func TestProviderAdaptersDecodeNativeFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider Provider
		fixture  string
		want     []string
	}{
		{ProviderClaude, "claude/output.ndjson", []string{
			"lifecycle.provider.initialized", "content.delta", "tool.call", "tool.result",
			"control.approval.request", "turn.failed", "turn.completed",
		}},
		{ProviderCodex, "codex/output.ndjson", []string{
			"control.response", "lifecycle.provider.session", "lifecycle.turn.started",
			"content.delta", "tool.call", "tool.result", "control.approval.request",
			"error.provider", "turn.completed",
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.provider), func(t *testing.T) {
			t.Parallel()
			adapter, err := NewAdapter(test.provider)
			if err != nil {
				t.Fatalf("new adapter: %v", err)
			}
			lines := readFixture(t, test.fixture)
			if len(lines) != len(test.want) {
				t.Fatalf("fixture records = %d, want %d", len(lines), len(test.want))
			}
			for index, line := range lines {
				original := append([]byte(nil), line...)
				derived, err := adapter.Decode(line)
				if err != nil {
					t.Fatalf("decode record %d: %v", index+1, err)
				}
				if derived.Type != test.want[index] {
					t.Errorf("record %d type = %q, want %q", index+1, derived.Type, test.want[index])
				}
				if !json.Valid(derived.Payload) {
					t.Errorf("record %d payload is invalid JSON: %s", index+1, derived.Payload)
				}
				if string(line) != string(original) {
					t.Errorf("record %d raw bytes were mutated", index+1)
				}
			}
		})
	}
}

func TestProviderAdaptersExposeProviderIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider Provider
		raw      string
		want     string
	}{
		{ProviderClaude, `{"type":"system","subtype":"init","session_id":"claude-session"}`, "claude-session"},
		{ProviderCodex, `{"method":"thread/started","params":{"threadId":"codex-thread"}}`, "codex-thread"},
	}
	for _, test := range tests {
		adapter, err := NewAdapter(test.provider)
		if err != nil {
			t.Fatalf("new adapter: %v", err)
		}
		derived, err := adapter.Decode([]byte(test.raw))
		if err != nil {
			t.Fatalf("decode %s: %v", test.provider, err)
		}
		if derived.ProviderID != test.want {
			t.Fatalf("%s provider ID = %q, want %q", test.provider, derived.ProviderID, test.want)
		}
	}
}

func TestNativeTransportContract(t *testing.T) {
	for _, provider := range []Provider{ProviderClaude, ProviderCodex} {
		provider := provider
		t.Run(string(provider), func(t *testing.T) {
			adapter, err := NewAdapter(provider)
			if err != nil {
				t.Fatalf("new adapter: %v", err)
			}
			process := newFakeProcess()
			transport := NewStreamTransport(adapter, process.IO(), RecoveryMetadata{
				SessionID: "session-1", Generation: 2, RuntimeID: "container-1",
			})
			if err := transport.Start(context.Background()); err != nil {
				t.Fatalf("start transport: %v", err)
			}
			input := Input{Kind: InputPrompt, Text: "hello", ProviderID: "provider-session"}
			if err := transport.Send(context.Background(), input); err != nil {
				t.Fatalf("send prompt: %v", err)
			}
			written := process.readInput(t)
			if !json.Valid(written) || !containsJSONText(written, "hello") {
				t.Fatalf("prompt input = %s", written)
			}

			raw := []byte(`{"fixture":"stdout"}`)
			process.writeStdout(t, raw)
			record := receiveRecord(t, transport.Records())
			if record.Stream != StreamProviderStdout || record.Ordinal != 1 || string(record.Raw) != string(raw) {
				t.Fatalf("stdout record = %+v", record)
			}
			stderrRaw := []byte("fixture stderr")
			process.writeStderr(t, stderrRaw)
			stderrRecord := receiveRecord(t, transport.Stderr())
			if stderrRecord.Stream != StreamRuntimeStderr || string(stderrRecord.Raw) != string(stderrRaw) {
				t.Fatalf("stderr record = %+v", stderrRecord)
			}
			if got := transport.RecoveryMetadata(); got.Generation != 2 || got.RuntimeID != "container-1" {
				t.Fatalf("recovery metadata = %+v", got)
			}
			if err := transport.Interrupt(context.Background()); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			if written := process.readInput(t); !json.Valid(written) {
				t.Fatalf("interrupt input is not JSON: %s", written)
			}

			process.finish(Exit{Code: 0})
			select {
			case exit := <-transport.Wait():
				if exit.Code != 0 {
					t.Fatalf("exit = %+v", exit)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for transport exit")
			}
		})
	}
}

func TestCodexTransportBootstrapsNewAndResumedThreads(t *testing.T) {
	for _, test := range []struct {
		name       string
		providerID string
		wantMethod string
	}{
		{name: "new", wantMethod: "thread/start"},
		{name: "resume", providerID: "existing-thread", wantMethod: "thread/resume"},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := NewAdapter(ProviderCodex)
			if err != nil {
				t.Fatalf("new adapter: %v", err)
			}
			process := newFakeProcess()
			transport := NewStreamTransport(adapter, process.IO(), RecoveryMetadata{SessionID: "bootstrap", Generation: 1})
			if err := transport.Start(context.Background()); err != nil {
				t.Fatalf("start transport: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				done <- transport.Bootstrap(context.Background(), BootstrapRequest{
					ProviderID: test.providerID, ClientName: "agentruntime", ClientVersion: "test",
				})
			}()

			initialize := decodeInputObject(t, process.readInput(t))
			if initialize["method"] != "initialize" {
				t.Fatalf("initialize message = %+v", initialize)
			}
			process.writeStdout(t, []byte(`{"id":0,"result":{"userAgent":"codex-test"}}`))
			initialized := decodeInputObject(t, process.readInput(t))
			if initialized["method"] != "initialized" {
				t.Fatalf("initialized message = %+v", initialized)
			}
			thread := decodeInputObject(t, process.readInput(t))
			if thread["method"] != test.wantMethod {
				t.Fatalf("thread method = %v, want %s", thread["method"], test.wantMethod)
			}
			threadID := test.providerID
			if threadID == "" {
				threadID = "new-thread"
			}
			process.writeStdout(t, []byte(fmt.Sprintf(`{"id":1,"result":{"threadId":%q}}`, threadID)))
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("bootstrap: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for bootstrap")
			}
			if err := transport.Send(context.Background(), Input{Kind: InputPrompt, Text: "hello"}); err != nil {
				t.Fatalf("send bootstrapped prompt: %v", err)
			}
			turn := decodeInputObject(t, process.readInput(t))
			params, _ := turn["params"].(map[string]any)
			if turn["method"] != "turn/start" || params["threadId"] != threadID {
				t.Fatalf("turn start = %+v", turn)
			}
			process.finish(Exit{Code: 0})
			<-transport.Wait()
		})
	}
}

func decodeInputObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode input object: %v", err)
	}
	return value
}

func readFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "testdata", "native-streams", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	var lines [][]byte
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return lines
}

func containsJSONText(raw []byte, want string) bool {
	return len(raw) > 0 && string(raw) != "" && string(raw) != want && containsBytes(raw, []byte(want))
}

func containsBytes(value, fragment []byte) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if string(value[index:index+len(fragment)]) == string(fragment) {
			return true
		}
	}
	return false
}

func receiveRecord(t *testing.T, records <-chan Record) Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for record")
		return Record{}
	}
}

type fakeProcess struct {
	inputs  chan []byte
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter
	wait    chan Exit
}

func newFakeProcess() *fakeProcess {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &fakeProcess{inputs: make(chan []byte, 8), stdoutR: stdoutR, stdoutW: stdoutW, stderrR: stderrR, stderrW: stderrW, wait: make(chan Exit, 1)}
}

func (process *fakeProcess) IO() ProcessIO {
	return ProcessIO{Stdin: &captureWriteCloser{writes: process.inputs}, Stdout: process.stdoutR, Stderr: process.stderrR, Wait: process.wait, Kill: func() error { return nil }}
}

func (process *fakeProcess) readInput(t *testing.T) []byte {
	t.Helper()
	select {
	case line := <-process.inputs:
		return line[:len(line)-1]
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transport input")
		return nil
	}
}

type captureWriteCloser struct {
	writes chan<- []byte
}

type errorReadCloser struct {
	err error
}

func (reader *errorReadCloser) Read([]byte) (int, error) { return 0, reader.err }

func (*errorReadCloser) Close() error { return nil }

func (writer *captureWriteCloser) Write(value []byte) (int, error) {
	writer.writes <- append([]byte(nil), value...)
	return len(value), nil
}

func (*captureWriteCloser) Close() error { return nil }

func (process *fakeProcess) writeStdout(t *testing.T, raw []byte) {
	t.Helper()
	if _, err := process.stdoutW.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
}

func (process *fakeProcess) writeStderr(t *testing.T, raw []byte) {
	t.Helper()
	if _, err := process.stderrW.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
}

func (process *fakeProcess) finish(exit Exit) {
	_ = process.stdoutW.Close()
	_ = process.stderrW.Close()
	process.wait <- exit
	close(process.wait)
}
