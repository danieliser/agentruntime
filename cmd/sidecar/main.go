package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const defaultPort = "9090"

func main() {
	server, port, err := newSidecarFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("sidecar listening on :%s (agent=%s)", port, server.AgentType())
	if err := http.ListenAndServe(":"+port, server.Routes()); err != nil {
		log.Fatal(err)
	}
}

func newSidecarFromEnv() (*ExternalWSServer, string, error) {
	cmd, err := parseAgentCommand(os.Getenv("AGENT_CMD"))
	if err != nil {
		return nil, "", err
	}

	port := os.Getenv("SIDECAR_PORT")
	if port == "" {
		port = defaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, "", errors.New("SIDECAR_PORT must be numeric")
	}

	agentType := detectAgentType(cmd)
	backend, err := newBackend(agentType, cmd)
	if err != nil {
		return nil, "", err
	}
	return NewExternalWSServer(agentType, backend), port, nil
}

func parseAgentCommand(raw string) ([]string, error) {
	if raw == "" {
		return nil, errors.New("AGENT_CMD is required")
	}

	var cmd []string
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		return nil, err
	}
	if len(cmd) == 0 || cmd[0] == "" {
		return nil, errors.New("AGENT_CMD must contain a command")
	}
	return cmd, nil
}

func detectAgentType(cmd []string) string {
	if len(cmd) == 0 {
		return "unknown"
	}

	name := strings.ToLower(filepath.Base(cmd[0]))
	switch {
	case strings.Contains(name, "claude"):
		return "claude"
	case strings.Contains(name, "codex"):
		return "codex"
	default:
		return name
	}
}

func newBackend(agentType string, cmd []string) (AgentBackend, error) {
	switch agentType {
	case "claude":
		return NewClaudeBackend(ClaudeBackendConfig{
			Binary: cmd[0],
		}), nil
	case "codex":
		return newCodexBackend(), nil
	default:
		return newUnsupportedBackend(agentType), nil
	}
}

type unsupportedBackend struct {
	agentType string
	sessionID string
	events    chan Event
	waitCh    chan int
}

func newUnsupportedBackend(agentType string) *unsupportedBackend {
	return &unsupportedBackend{
		agentType: agentType,
		sessionID: uuid.NewString(),
		events:    make(chan Event),
		waitCh:    make(chan int),
	}
}

func (b *unsupportedBackend) Start(context.Context) error { return nil }

func (b *unsupportedBackend) SendPrompt(string) error {
	return errors.New("prompt routing is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendInterrupt() error {
	return errors.New("interrupt routing is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendSteer(string) error {
	return errors.New("steering is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendContext(string, string) error {
	return errors.New("context injection is not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) SendMention(string, int, int) error {
	return errors.New("mentions are not implemented for " + b.agentType + " yet")
}

func (b *unsupportedBackend) Events() <-chan Event { return b.events }
func (b *unsupportedBackend) SessionID() string    { return b.sessionID }
func (b *unsupportedBackend) Running() bool        { return false }
func (b *unsupportedBackend) Wait() <-chan int     { return b.waitCh }
func (b *unsupportedBackend) Close() error         { return nil }
