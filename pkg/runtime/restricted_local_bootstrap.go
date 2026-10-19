package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/materialize"
)

// probeCodexBootstrap never sends turn/start or user input. The real server must
// parse strict config, load ancestor instructions, and confirm the named profile.
func probeCodexBootstrap(ctx context.Context, binary, work string, env []string) error {
	isolation, err := materialize.RestrictedLocalCodexArgs(work)
	if err != nil {
		return err
	}
	args := append([]string{"app-server", "--listen", "stdio://", "--strict-config", "-c", `approval_policy="on-request"`, "-c", `default_permissions="agentd-local-v1"`}, isolation...)
	cmd := restrictedLocalProbeCommand(ctx, binary, args...)
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
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { stdin.Close(); killLocalProcessGroup(cmd); cmd.Wait() }() // Expected teardown of a no-turn bootstrap.
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	exchange := func(id int, method string, params any) (json.RawMessage, error) {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for scanner.Scan() {
			var msg struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				return nil, err
			}
			if msg.ID == nil || *msg.ID != id {
				continue
			}
			if len(msg.Error) > 0 && string(msg.Error) != "null" {
				return nil, fmt.Errorf("%s rejected", method)
			}
			return msg.Result, nil
		}
		return nil, errors.Join(errors.New("bootstrap stream ended"), scanner.Err(), ctx.Err())
	}
	result, err := exchange(0, "initialize", map[string]any{"clientInfo": map[string]string{"name": "agentruntime", "version": "qualification"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err != nil {
		return err
	}
	var init struct {
		UserAgent string `json:"userAgent"`
	}
	if json.Unmarshal(result, &init) != nil || init.UserAgent == "" {
		return errors.New("missing initialize user agent")
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	result, err = exchange(1, "thread/start", map[string]any{"cwd": work, "permissions": apischema.RestrictedLocalCodexProfile, "approvalsReviewer": "user", "approvalPolicy": "on-request", "dynamicTools": []any{}})
	if err != nil {
		return err
	}
	var thread struct {
		Active struct {
			ID string `json:"id"`
		} `json:"activePermissionProfile"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		ID string `json:"threadId"`
	}
	if json.Unmarshal(result, &thread) != nil || thread.Active.ID != apischema.RestrictedLocalCodexProfile || (thread.Thread.ID == "" && thread.ID == "") {
		return errors.New("bootstrap did not confirm active permission profile and thread")
	}
	return nil
}
