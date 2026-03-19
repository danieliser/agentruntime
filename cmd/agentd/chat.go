package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func runChatCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agentd chat <create|send|list|delete> [options]")
		return 2
	}
	switch args[0] {
	case "create":
		return runChatCreate(args[1:])
	case "send":
		return runChatSend(args[1:])
	case "list":
		return runChatList(args[1:])
	case "delete":
		return runChatDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agentd chat: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runChatCreate(args []string) int {
	fs := flag.NewFlagSet("chat create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	port := fs.Int("port", 8090, "Daemon port")
	agentFlag := fs.String("agent", "", "Agent name (required unless --config is given)")
	runtimeFlag := fs.String("runtime", "", "Execution runtime (local|docker)")
	model := fs.String("model", "", "Model name")
	effort := fs.String("effort", "", "Effort level")
	workDir := fs.String("work-dir", "", "Working directory")
	idleTimeout := fs.String("idle-timeout", "", "Idle timeout (e.g. 30m)")
	configPath := fs.String("config", "", "Path to YAML file with ChatConfig (mutually exclusive with individual flags)")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: agentd chat create <name> [options]\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if fs.NArg() < 1 {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "chat create: name is required")
		return 2
	}
	name := fs.Arg(0)

	var cfg apischema.ChatAPIConfig
	if *configPath != "" {
		data, err := os.ReadFile(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chat create: read config: %v\n", err)
			return 1
		}
		// YAML → map → JSON → struct handles snake_case keys correctly.
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			fmt.Fprintf(os.Stderr, "chat create: parse config: %v\n", err)
			return 1
		}
		jsonData, _ := json.Marshal(raw)
		if err := json.Unmarshal(jsonData, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "chat create: decode config: %v\n", err)
			return 1
		}
	} else {
		if *agentFlag == "" {
			fmt.Fprintln(os.Stderr, "chat create: --agent is required")
			return 2
		}
		cfg = apischema.ChatAPIConfig{
			Agent:       *agentFlag,
			Runtime:     *runtimeFlag,
			Model:       *model,
			Effort:      *effort,
			WorkDir:     *workDir,
			IdleTimeout: *idleTimeout,
		}
	}

	resp, err := chatPost(*port, "/chats", apischema.CreateChatRequest{Name: name, Config: cfg})
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat create: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusCreated:
		// success — decode below
	case http.StatusConflict:
		fmt.Fprintf(os.Stderr, "chat create: chat %q already exists\n", name)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "chat create: server error %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	var created apischema.ChatResponse
	if err := json.Unmarshal(body, &created); err != nil {
		fmt.Fprintf(os.Stderr, "chat create: decode response: %v\n", err)
		return 1
	}

	if created.VolumeName != "" {
		fmt.Printf("Created chat %q (volume: %s)\n", created.Name, created.VolumeName)
	} else {
		fmt.Printf("Created chat %q\n", created.Name)
	}
	return 0
}

func runChatSend(args []string) int {
	fs := flag.NewFlagSet("chat send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	port := fs.Int("port", 8090, "Daemon port")
	follow := fs.Bool("follow", false, "Stream output after sending")
	fs.BoolVar(follow, "f", false, "Stream output after sending (shorthand)")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: agentd chat send <name> <message> [--follow]\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if fs.NArg() < 2 {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "chat send: name and message are required")
		return 2
	}
	name := fs.Arg(0)
	message := fs.Arg(1)

	resp, err := chatPost(*port, "/chats/"+name+"/messages", apischema.SendMessageRequest{Message: message})
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat send: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusAccepted:
		// success — decode below
	case http.StatusTooManyRequests:
		var errBody struct {
			RetryAfterMs int `json:"retry_after_ms"`
		}
		_ = json.Unmarshal(body, &errBody)
		fmt.Fprintf(os.Stderr, "Chat is busy. Retry after %dms.\n", errBody.RetryAfterMs)
		return 1
	case http.StatusNotFound:
		fmt.Fprintf(os.Stderr, "chat send: chat %q not found\n", name)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "chat send: server error %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	var sendResp apischema.SendMessageResponse
	if err := json.Unmarshal(body, &sendResp); err != nil {
		fmt.Fprintf(os.Stderr, "chat send: decode response: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "session=%s state=%s\n", sendResp.SessionID, sendResp.State)

	if *follow && sendResp.SessionID != "" {
		if err := attach(sendResp.SessionID, *port, 0, false); err != nil {
			fmt.Fprintf(os.Stderr, "chat send: attach: %v\n", err)
			return 1
		}
	}

	return 0
}

func runChatList(args []string) int {
	fs := flag.NewFlagSet("chat list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	port := fs.Int("port", 8090, "Daemon port")
	asJSON := fs.Bool("json", false, "Output raw JSON")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: agentd chat list [--json]\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	resp, err := chatGet(*port, "/chats")
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat list: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat list: read response: %v\n", err)
		return 1
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "chat list: server error %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	if *asJSON {
		fmt.Println(string(body))
		return 0
	}

	var summaries []apischema.ChatSummary
	if err := json.Unmarshal(body, &summaries); err != nil {
		fmt.Fprintf(os.Stderr, "chat list: decode response: %v\n", err)
		return 1
	}

	if len(summaries) == 0 {
		fmt.Println("No chats.")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tAGENT\tSESSIONS\tLAST ACTIVE")
	for _, s := range summaries {
		lastActive := "-"
		if s.LastActiveAt != nil {
			lastActive = s.LastActiveAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", s.Name, s.State, s.Agent, s.SessionCount, lastActive)
	}
	w.Flush()
	return 0
}

func runChatDelete(args []string) int {
	fs := flag.NewFlagSet("chat delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	port := fs.Int("port", 8090, "Daemon port")
	removeVolume := fs.Bool("remove-volume", false, "Remove the Docker volume")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: agentd chat delete <name> [--remove-volume]\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if fs.NArg() < 1 {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "chat delete: name is required")
		return 2
	}
	name := fs.Arg(0)

	path := "/chats/" + name
	if *removeVolume {
		path += "?remove_volume=true"
	}

	resp, err := chatDelete(*port, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat delete: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		// success
	case http.StatusNotFound:
		fmt.Fprintf(os.Stderr, "chat delete: chat %q not found\n", name)
		return 1
	default:
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "chat delete: server error %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	fmt.Printf("Deleted chat %q\n", name)
	return 0
}

// chatPost sends a POST request with a JSON body to the daemon.
func chatPost(port int, path string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	url := fmt.Sprintf("http://localhost:%d%s", port, path)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data)) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	return resp, nil
}

// chatGet sends a GET request to the daemon.
func chatGet(port int, path string) (*http.Response, error) {
	url := fmt.Sprintf("http://localhost:%d%s", port, path)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return resp, nil
}

// chatDelete sends a DELETE request to the daemon.
func chatDelete(port int, path string) (*http.Response, error) {
	url := fmt.Sprintf("http://localhost:%d%s", port, path)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DELETE %s: %w", path, err)
	}
	return resp, nil
}
