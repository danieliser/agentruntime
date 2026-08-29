package materialize

import (
	"encoding/json"
	"strings"
	"testing"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestPrepareLocalMCP_ClaudeUsesScopedConfigAndLoopbackGateway(t *testing.T) {
	const token = "local-claude-token"
	req := &apischema.SessionRequest{
		Agent: "claude",
		MCPServers: []apischema.MCPServer{{
			Name: "persist", Type: "http",
			URL: "http://${HOST_GATEWAY}:8803/api/v1/mcp", Token: token,
		}},
	}
	args, err := PrepareLocalMCP(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 || args[0] != "--strict-mcp-config" || args[1] != "--mcp-config" {
		t.Fatalf("Claude local MCP args = %#v", args)
	}
	if strings.Contains(args[2], token) {
		t.Fatal("plaintext token reached Claude argv")
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(args[2]), &config); err != nil {
		t.Fatal(err)
	}
	server := config["mcpServers"].(map[string]any)["persist"].(map[string]any)
	if got := server["url"]; got != "http://127.0.0.1:8803/api/v1/mcp" {
		t.Fatalf("local URL = %v", got)
	}
	assertReferencedTokenEnvironment(t, req, server["headers"].(map[string]any)["Authorization"].(string), token)
}

func TestPrepareLocalMCP_CodexUsesConfigOverridesAndInheritedHome(t *testing.T) {
	const token = "local-codex-token"
	req := &apischema.SessionRequest{
		Agent: "codex",
		MCPServers: []apischema.MCPServer{{
			Name: "persist", Type: "http",
			URL: "http://${HOST_GATEWAY}:8803/api/v1/mcp", Token: token,
		}},
	}
	args, err := PrepareLocalMCP(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, token) {
		t.Fatal("plaintext token reached Codex argv")
	}
	for _, want := range []string{
		`mcp_servers.persist.url="http://127.0.0.1:8803/api/v1/mcp"`,
		`mcp_servers.persist.bearer_token_env_var="AGENTRUNTIME_MCP_TOKEN_`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex local MCP args %q do not contain %q", joined, want)
		}
	}
	if _, found := req.Env["HOME"]; found {
		t.Fatal("local MCP preparation must not replace HOME or hide ambient AutoVault skills")
	}
}

func TestPrepareLocalMCP_CodexStdioSeparatesCommandAndArgsDeterministically(t *testing.T) {
	req := &apischema.SessionRequest{
		Agent: "codex",
		MCPServers: []apischema.MCPServer{{
			Name: "worker", Type: "stdio",
			Cmd: []string{"node", "server.js", "--stdio"},
			Env: map[string]string{"Z_LAST": "z", "A_FIRST": "a"},
		}},
	}
	args, err := PrepareLocalMCP(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		`mcp_servers.worker.command="node"`,
		`mcp_servers.worker.args=["server.js","--stdio"]`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex local MCP args %q do not contain %q", joined, want)
		}
	}
	if strings.Contains(joined, `command=[`) {
		t.Fatalf("Codex command must be a scalar, got %q", joined)
	}
	if strings.Index(joined, "A_FIRST") > strings.Index(joined, "Z_LAST") {
		t.Fatalf("Codex MCP env overrides are not deterministic: %q", joined)
	}
}

func assertReferencedTokenEnvironment(t *testing.T, req *apischema.SessionRequest, authorization, token string) {
	t.Helper()
	envName := strings.TrimSuffix(strings.TrimPrefix(authorization, "Bearer ${"), "}")
	if !strings.HasPrefix(envName, mcpTokenEnvPrefix) {
		t.Fatalf("authorization env reference = %q", authorization)
	}
	if got := req.Env[envName]; got != token {
		t.Fatalf("referenced token env = %q", got)
	}
}
