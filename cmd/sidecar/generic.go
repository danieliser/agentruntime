package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

type genericCommandBackend struct {
	agentType string
	cmd       []string
	prompt    string
	sessionID string

	mu      sync.RWMutex
	process *exec.Cmd
	stdin   io.WriteCloser
	running bool
	started bool

	startOnce sync.Once
	closeOnce sync.Once

	events chan Event
	waitCh chan backendExit
	done   chan struct{}
}

func newGenericCommandBackend(agentType string, cmd []string, prompt string) *genericCommandBackend {
	return &genericCommandBackend{
		agentType: agentType,
		cmd:       append([]string(nil), cmd...),
		prompt:    prompt,
		sessionID: uuid.NewString(),
		events:    make(chan Event, 64),
		waitCh:    make(chan backendExit, 1),
		done:      make(chan struct{}),
	}
}

func (b *genericCommandBackend) Start(ctx context.Context) error {
	var startErr error

	b.startOnce.Do(func() {
		if len(b.cmd) == 0 || strings.TrimSpace(b.cmd[0]) == "" {
			startErr = errors.New("generic backend command is required")
			return
		}

		cmd := exec.CommandContext(ctx, b.cmd[0], b.cmd[1:]...)
		cmd.Env = buildCleanEnv(nil)

		stdin, err := cmd.StdinPipe()
		if err != nil {
			startErr = err
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			startErr = err
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			startErr = err
			return
		}
		if err := cmd.Start(); err != nil {
			startErr = err
			return
		}

		b.mu.Lock()
		b.process = cmd
		b.stdin = stdin
		b.running = true
		b.started = true
		b.mu.Unlock()

		go b.readStream(stdout, "stdout")
		go b.readStream(stderr, "stderr")
		go b.waitLoop()

		if prompt := strings.TrimSpace(b.prompt); prompt != "" {
			go func() {
				if err := b.SendPrompt(prompt); err != nil {
					b.emit(Event{
						Type: "error",
						Data: map[string]any{"message": err.Error()},
					})
				}
				b.closeInput()
			}()
		}
	})

	return startErr
}

func (b *genericCommandBackend) SendPrompt(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return errors.New("prompt content is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stdin == nil {
		return errors.New("generic backend stdin unavailable")
	}
	_, err := io.WriteString(b.stdin, content+"\n")
	return err
}

func (b *genericCommandBackend) SendInterrupt() error {
	b.mu.RLock()
	process := b.process
	b.mu.RUnlock()

	if process == nil || process.Process == nil {
		return nil
	}

	if err := process.Process.Signal(os.Interrupt); err == nil {
		return nil
	} else if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return process.Process.Signal(syscall.SIGKILL)
}

func (b *genericCommandBackend) SendSteer(content string) error {
	return b.SendPrompt(content)
}

func (b *genericCommandBackend) SendContext(string, string) error {
	return errors.New("context injection is not implemented for " + b.agentType + " yet")
}

func (b *genericCommandBackend) SendMention(string, int, int) error {
	return errors.New("mentions are not implemented for " + b.agentType + " yet")
}

func (b *genericCommandBackend) Events() <-chan Event { return b.events }
func (b *genericCommandBackend) SessionID() string    { return b.sessionID }

func (b *genericCommandBackend) Running() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

func (b *genericCommandBackend) Wait() <-chan backendExit { return b.waitCh }

func (b *genericCommandBackend) Close() error {
	var closeErr error

	b.closeOnce.Do(func() {
		b.mu.Lock()
		process := b.process
		started := b.started
		b.process = nil
		b.stdin = nil
		b.running = false
		b.mu.Unlock()

		if process != nil && process.Process != nil {
			if err := process.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				closeErr = err
			}
		} else if !started {
			close(b.waitCh)
			close(b.events)
			close(b.done)
		}
	})

	return closeErr
}

func (b *genericCommandBackend) closeInput() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stdin != nil {
		_ = b.stdin.Close()
		b.stdin = nil
	}
}

func (b *genericCommandBackend) readStream(r io.ReadCloser, eventType string) {
	if r == nil {
		return
	}
	defer r.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		text := strings.TrimRight(scanner.Text(), "\r\n")
		if text == "" {
			continue
		}
		b.emit(Event{
			Type: eventType,
			Data: map[string]any{"text": text},
		})
	}
	if err := scanner.Err(); err != nil && !b.isClosed() {
		b.emit(Event{
			Type: "error",
			Data: map[string]any{"message": err.Error()},
		})
	}
}

func (b *genericCommandBackend) waitLoop() {
	b.mu.RLock()
	process := b.process
	b.mu.RUnlock()
	if process == nil {
		return
	}

	err := process.Wait()
	code := 0
	detail := ""
	if err != nil {
		code = 1
		detail = err.Error()
	}

	b.mu.Lock()
	b.running = false
	b.stdin = nil
	b.mu.Unlock()

	select {
	case b.waitCh <- backendExit{Code: code, ErrorDetail: detail}:
	default:
	}

	b.closeOnce.Do(func() {
		close(b.waitCh)
		close(b.events)
		close(b.done)
	})
}

func (b *genericCommandBackend) emit(event Event) {
	select {
	case <-b.done:
		return
	case b.events <- event:
	default:
		b.events <- event
	}
}

func (b *genericCommandBackend) isClosed() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}
