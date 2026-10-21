package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/materialize"
)

func probeRestrictedLocalClaude(ctx context.Context, binary, root string) (string, error) {
	return measureRestrictedLocalClaude(ctx, binary, root, nil)
}

// No production Bash execution entry point is exposed without a model. Do not
// replace that evidence with a hand-written Seatbelt profile: qualify only file
// tools, requiring effective settings/rules and restricted-mode bypass refusal.
func measureRestrictedLocalClaude(ctx context.Context, binary, root string, passed func(string)) (version string, err error) {
	if err = os.MkdirAll(root, 0700); err != nil {
		return
	}
	fixture, err := os.MkdirTemp(root, ".restricted-claude-probe-")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(fixture)) }()
	work := filepath.Join(fixture, "project.repo", "my.app")
	storage := filepath.Join(fixture, "agentd")
	for _, path := range []string{work, storage, filepath.Join(fixture, "version-home")} {
		if err = os.MkdirAll(path, 0700); err != nil {
			return
		}
	}
	versionHome := filepath.Join(fixture, "version-home")
	versionEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + versionHome, "CLAUDE_CONFIG_DIR=" + versionHome, "TMPDIR=" + versionHome, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1"}
	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := restrictedLocalProbeCommand(versionCtx, binary, "--version")
	cmd.Env = versionEnv
	out, runErr := cmd.Output()
	version = strings.TrimSpace(string(out))
	if runErr != nil || !strings.HasSuffix(version, " (Claude Code)") {
		return version, errors.New("cannot measure Claude version")
	}
	helpCtx, helpCancel := context.WithTimeout(ctx, 3*time.Second)
	defer helpCancel()
	cmd = restrictedLocalProbeCommand(helpCtx, binary, "--help")
	cmd.Env = versionEnv
	help, helpErr := cmd.Output()
	if helpErr != nil {
		return version, errors.New("cannot inspect Claude restrictions")
	}
	for _, text := range []string{"--restricted", "confines the file tools", "--permission-prompts", "--strict-mcp-config", "--tools"} {
		if !strings.Contains(string(help), text) {
			return version, errors.New("Claude lacks required restricted interface")
		}
	}
	if passed != nil {
		passed("restricted-interface")
	}
	for _, policy := range []string{"on-request", "never"} {
		mode := "default"
		if policy == "never" {
			mode = "dontAsk"
		}
		req := &apischema.SessionRequest{Agent: "claude", Runtime: "local", WorkDir: work, ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: policy}}
		args := append([]string{binary}, materialize.RestrictedLocalClaudeArgs(mode)...)
		command, env, cleanup, prepareErr := materializeRestrictedLocal(SpawnConfig{SessionID: "probe-" + policy, DataDir: storage, WorkDir: work, Cmd: args, Request: req})
		if cleanup != nil {
			defer func() { err = errors.Join(err, cleanup()) }()
		}
		if prepareErr != nil {
			return version, prepareErr
		}
		expected, readErr := os.ReadFile(command[len(command)-1])
		if readErr != nil {
			return version, readErr
		}
		if bootstrapErr := probeClaudeBootstrap(ctx, command, work, env, expected, version, mode, passed); bootstrapErr != nil {
			return version, fmt.Errorf("Claude %s qualification: %w", policy, bootstrapErr)
		}
	}
	return version, nil
}

// Only control requests are sent. No user/assistant message, turn or model call.
func probeClaudeBootstrap(ctx context.Context, command []string, work string, env []string, settings []byte, version, mode string, passed func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cmd := restrictedLocalProbeCommand(ctx, command[0], command[1:]...)
	cmd.Env = env
	cmd.Dir = work
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	defer func() { stdin.Close(); killLocalProcessGroup(cmd); cmd.Wait() }() // No-turn child teardown.
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	exchange := func(id, subtype string, request map[string]string) (json.RawMessage, string, error) {
		request["subtype"] = subtype
		if err := encoder.Encode(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
			return nil, "", err
		}
		for scanner.Scan() {
			var msg struct {
				Type     string `json:"type"`
				Response struct {
					Subtype string          `json:"subtype"`
					ID      string          `json:"request_id"`
					Result  json.RawMessage `json:"response"`
					Code    string          `json:"error_code"`
				} `json:"response"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) != nil {
				return nil, "", errors.New("invalid Claude bootstrap response")
			}
			if msg.Type == "control_response" && msg.Response.ID == id {
				if msg.Response.Subtype == "error" {
					return nil, msg.Response.Code, nil
				}
				if msg.Response.Subtype != "success" {
					return nil, "", errors.New("invalid control response")
				}
				return msg.Response.Result, "", nil
			}
		}
		return nil, "", errors.Join(errors.New("Claude bootstrap ended before confirmation"), scanner.Err(), ctx.Err())
	}
	record := func(name string) {
		if passed != nil {
			passed(mode + "/" + name)
		}
	}
	result, code, err := exchange("init", "initialize", map[string]string{})
	if err != nil || code != "" {
		return errors.Join(errors.New("initialize rejected"), err)
	}
	var init struct {
		Version  string            `json:"claude_code_version"`
		Mode     string            `json:"current_permission_mode"`
		Commands []json.RawMessage `json:"commands"`
	}
	if json.Unmarshal(result, &init) != nil || init.Version+" (Claude Code)" != version || init.Mode != mode || len(init.Commands) != 0 {
		return errors.New("Claude did not confirm version/mode/disabled commands")
	}
	record("no-turn-initialize")
	result, code, err = exchange("settings", "get_settings", map[string]string{})
	if err != nil || code != "" {
		return errors.Join(errors.New("effective settings unavailable"), err)
	}
	var applied struct {
		Effective json.RawMessage   `json:"effective"`
		Errors    []json.RawMessage `json:"errors"`
	}
	var want, got any
	if json.Unmarshal(result, &applied) != nil {
		return errors.New("invalid effective settings response")
	}
	if len(applied.Errors) != 0 {
		return errors.New("Claude reported settings validation errors")
	}
	if json.Unmarshal(settings, &want) != nil || json.Unmarshal(applied.Effective, &got) != nil || !reflect.DeepEqual(want, got) {
		return errors.New("effective settings differ from generated restrictions")
	}
	record("effective-sandbox-filesystem-network-settings")
	record("effective-write-approval-rules")
	result, code, err = exchange("rules", "list_permission_rules", map[string]string{})
	if err != nil || code != "" {
		return errors.Join(errors.New("permission rule readback unavailable"), err)
	}
	if err = validateClaudeFileToolRules(result); err != nil {
		return err
	}
	record("command-web-agent-tools-denied")
	result, code, err = exchange("bypass", "set_permission_mode", map[string]string{"mode": "bypassPermissions"})
	if err != nil || code != "bypass_restricted" {
		return errors.Join(errors.New("restricted bypass was not refused"), err)
	}
	record("bypass-refused")
	if err = stdin.Close(); err != nil {
		return err
	}
	// Closing input without a user message must exit cleanly under the deadline.
	if err = cmd.Wait(); err != nil {
		return errors.New("Claude no-turn exit failed")
	}
	record("stdin-close-no-turn-exit")
	return nil
}

func validateClaudeFileToolRules(raw json.RawMessage) error {
	var response struct {
		State struct {
			Rules []struct {
				Behavior string `json:"behavior"`
				Rule     string `json:"rule"`
			} `json:"rules"`
		} `json:"state"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return errors.New("invalid permission rule readback")
	}
	denied := map[string]bool{}
	asked := map[string]bool{}
	for _, rule := range response.State.Rules {
		if rule.Behavior == "allow" {
			return errors.New("unexpected automatic tool grant")
		}
		if rule.Behavior == "deny" {
			denied[rule.Rule] = true
		}
		if rule.Behavior == "ask" {
			asked[rule.Rule] = true
		}
	}
	for _, tool := range []string{"Bash", "Agent", "WebFetch", "WebSearch", "NotebookEdit"} {
		if !denied[tool] {
			return errors.New("command/network/agent tool not denied")
		}
	}
	for _, tool := range []string{"Edit", "Write", "Bash"} {
		if !asked[tool] {
			return errors.New("write/command approval missing")
		}
	}
	for _, tool := range []string{"Read", "Glob", "Grep", "Edit", "Write"} {
		if denied[tool] {
			return errors.New("required file tool unavailable")
		}
	}
	return nil
}
