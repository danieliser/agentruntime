package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const defaultURL = "http://127.0.0.1:38093"

type Config struct {
	URL    string
	Caller string
	JSON   bool
	Raw    bool
	Token  string `json:"-"`
}

type Invocation struct {
	Config  Config
	Command string
	Args    []string
}

var commandNames = []string{"run", "status", "since", "grep", "steer", "prompt", "interrupt", "cancel", "terminate", "resume", "approvals", "approve", "deny", "wait", "result", "ls"}

// ParseArgs accepts global flags on either side of a subcommand. Values are
// captured in Config and never retained in the argument vector.
func ParseArgs(args []string, environ map[string]string) (Invocation, error) {
	cfg := Config{URL: envOr(environ["AGENTD_URL"], defaultURL), Caller: envOr(environ["AGENTD_CALLER"], "agentctl")}
	fs := flag.NewFlagSet("agentctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.URL, "url", cfg.URL, "AgentD base URL")
	fs.StringVar(&cfg.Caller, "as", cfg.Caller, "caller name")
	fs.BoolVar(&cfg.JSON, "json", false, "JSON output")
	fs.BoolVar(&cfg.Raw, "raw", false, "raw event output")
	global := make([]string, 0, 8)
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		key, _, hasValue := strings.Cut(name, "=")
		switch key {
		case "url", "as":
			if !hasValue {
				i++
				if i >= len(args) {
					return Invocation{}, fmt.Errorf("missing value for --%s", key)
				}
				a += "=" + args[i]
			}
			global = append(global, a)
		case "json", "raw":
			global = append(global, a)
		default:
			positional = append(positional, a)
		}
	}
	parsed := append([]string(nil), global...)
	if len(positional) > 0 {
		parsed = append(parsed, "--")
		parsed = append(parsed, positional...)
	}
	if err := fs.Parse(parsed); err != nil {
		return Invocation{}, fmt.Errorf("%w: invalid flags", ErrUsage)
	}
	pos := fs.Args()
	if len(pos) == 0 {
		return Invocation{}, fmt.Errorf("%w: missing command", ErrUsage)
	}
	if _, ok := commandRegistry[pos[0]]; !ok {
		return Invocation{}, fmt.Errorf("%w: unknown command %q", ErrUsage, pos[0])
	}
	if strings.TrimSpace(cfg.URL) == "" {
		return Invocation{}, fmt.Errorf("%w: empty URL", ErrUsage)
	}
	return Invocation{Config: cfg, Command: pos[0], Args: append([]string(nil), pos[1:]...)}, nil
}

func envOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func LoadToken(environ map[string]string) (string, error) {
	path := environ["AGENTD_TOKEN_FILE"]
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("cannot determine home directory for AgentD token")
		}
		path = filepath.Join(home, ".agentd", "auth.token")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read AgentD bearer token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("AgentD bearer token file is empty")
	}
	return token, nil
}
