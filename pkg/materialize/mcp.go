package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

const mcpTokenEnvPrefix = "AGENTRUNTIME_MCP_TOKEN_"

// PrepareLocalMCP returns provider-native command arguments for the explicit
// MCP set while leaving HOME untouched so local sessions retain their ambient
// provider configuration and AutoVault-managed skills.
func PrepareLocalMCP(req *apischema.SessionRequest) ([]string, error) {
	if req == nil || len(req.MCPServers) == 0 {
		return nil, nil
	}

	switch req.Agent {
	case "claude":
		servers := make(map[string]any, len(req.MCPServers))
		for _, server := range req.MCPServers {
			resolved, err := localClaudeMCPServer(req, server)
			if err != nil {
				return nil, err
			}
			servers[server.Name] = resolved
		}
		payload, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return nil, fmt.Errorf("marshal local Claude MCP config: %w", err)
		}
		return []string{"--strict-mcp-config", "--mcp-config", string(payload)}, nil
	case "codex":
		args := make([]string, 0, len(req.MCPServers)*4)
		for _, server := range req.MCPServers {
			serverArgs, err := localCodexMCPArgs(req, server)
			if err != nil {
				return nil, err
			}
			args = append(args, serverArgs...)
		}
		return args, nil
	default:
		return nil, fmt.Errorf("local MCP servers are unsupported for agent %q", req.Agent)
	}
}

func localClaudeMCPServer(req *apischema.SessionRequest, server apischema.MCPServer) (map[string]any, error) {
	resolved := map[string]any{"type": server.Type}
	if server.URL != "" {
		url := sanitizeLocalMCPURL(server.URL)
		if url == "" {
			return nil, fmt.Errorf("local MCP server %q has an invalid URL", server.Name)
		}
		resolved["url"] = url
	}
	if len(server.Cmd) > 0 {
		resolved["command"] = server.Cmd[0]
		if len(server.Cmd) > 1 {
			resolved["args"] = append([]string(nil), server.Cmd[1:]...)
		}
	}
	if len(server.Env) > 0 {
		resolved["env"] = server.Env
	}
	if server.Token != "" {
		envName := injectMCPTokenEnv(req, server)
		resolved["headers"] = map[string]any{"Authorization": "Bearer ${" + envName + "}"}
	}
	return resolved, nil
}

func localCodexMCPArgs(req *apischema.SessionRequest, server apischema.MCPServer) ([]string, error) {
	if !validMCPName(server.Name) {
		return nil, fmt.Errorf("local MCP server name %q is not a safe Codex config key", server.Name)
	}
	prefix := "mcp_servers." + server.Name + "."
	args := make([]string, 0, 8)
	if server.URL != "" {
		url := sanitizeLocalMCPURL(server.URL)
		if url == "" {
			return nil, fmt.Errorf("local MCP server %q has an invalid URL", server.Name)
		}
		args = append(args, "-c", prefix+"url="+strconv.Quote(url))
	}
	if len(server.Cmd) > 0 {
		args = append(args, "-c", prefix+"command="+strconv.Quote(server.Cmd[0]))
		quoted := make([]string, len(server.Cmd)-1)
		for index, part := range server.Cmd[1:] {
			quoted[index] = strconv.Quote(part)
		}
		if len(quoted) > 0 {
			args = append(args, "-c", prefix+"args=["+strings.Join(quoted, ",")+"]")
		}
	}
	envKeys := make([]string, 0, len(server.Env))
	for key := range server.Env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		value := server.Env[key]
		args = append(args, "-c", prefix+"env."+strconv.Quote(key)+"="+strconv.Quote(value))
	}
	if server.Token != "" {
		envName := injectMCPTokenEnv(req, server)
		args = append(args, "-c", prefix+"bearer_token_env_var="+strconv.Quote(envName))
	}
	return args, nil
}

func validMCPName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func sanitizeLocalMCPURL(raw string) string {
	return sanitizeMCPURL(strings.ReplaceAll(raw, hostGatewayVar, "127.0.0.1"))
}

func injectMCPTokenEnv(req *apischema.SessionRequest, server apischema.MCPServer) string {
	sum := sha256.Sum256([]byte(server.Name))
	envName := mcpTokenEnvPrefix + hex.EncodeToString(sum[:8])
	if req.Env == nil {
		req.Env = make(map[string]string)
	}
	req.Env[envName] = sanitizeMCPToken(server.Token)
	return envName
}

func mergeCodexMCPServers(req *apischema.SessionRequest, config map[string]any) {
	if len(req.MCPServers) == 0 {
		return
	}

	servers, _ := cloneValue(config["mcp_servers"]).(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}
	for _, server := range req.MCPServers {
		resolved := make(map[string]any)
		if server.URL != "" {
			if url := sanitizeMCPURL(server.URL); url != "" {
				resolved["url"] = url
			}
		}
		if len(server.Cmd) > 0 {
			resolved["command"] = server.Cmd[0]
			if len(server.Cmd) > 1 {
				resolved["args"] = append([]string(nil), server.Cmd[1:]...)
			}
		}
		if len(server.Env) > 0 {
			env := make(map[string]any, len(server.Env))
			for key, value := range server.Env {
				env[key] = value
			}
			resolved["env"] = env
		}
		if server.Token != "" {
			resolved["bearer_token_env_var"] = injectMCPTokenEnv(req, server)
		}
		servers[server.Name] = resolved
	}
	config["mcp_servers"] = servers
}
