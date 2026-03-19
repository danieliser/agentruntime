# Configuring MCP Servers in agentruntime

This guide covers how to configure Model Context Protocol (MCP) servers to extend agent capabilities in agentruntime sessions.

## MCPServer Fields

Every MCP server configuration requires a `name` and `type`, plus additional fields depending on the server implementation:

### `name` (required)
Unique identifier for the server. Used as the key in the agent's MCP configuration.

```json
{
  "name": "my-filesystem-server"
}
```

### `type` (required)
Specifies how the agent communicates with the server. Must be one of:

- **`http`** — Expose the server via HTTP or HTTPS. Requires `url` field.
- **`stdio`** — Launch the server as a subprocess that communicates over stdin/stdout. Requires `cmd` field.
- **`websocket`** — Expose the server via WebSocket. Requires `url` field.

```json
{
  "name": "web-server",
  "type": "http"
}
```

### `url` (optional, required for http/websocket)
The network address where the MCP server is reachable. Supports `http`, `https`, `ws`, and `wss` schemes.

Supports the `${HOST_GATEWAY}` placeholder to reference the Docker host gateway (resolved at session materialization time).

```json
{
  "name": "local-http-server",
  "type": "http",
  "url": "http://localhost:3000/mcp"
}
```

```json
{
  "name": "local-server-from-container",
  "type": "http",
  "url": "http://${HOST_GATEWAY}:3000/mcp"
}
```

Invalid schemes (e.g., `ftp://`, custom protocols) are silently dropped during materialization.

### `cmd` (optional, required for stdio)
Command to launch the MCP server as a subprocess. Specified as a JSON array of strings: the executable path followed by any arguments.

```json
{
  "name": "filesystem-server",
  "type": "stdio",
  "cmd": ["node", "/opt/servers/filesystem-server/dist/index.js"]
}
```

### `env` (optional)
Environment variables passed to the subprocess (stdio type) or available to the agent process. Specified as a JSON object mapping variable names to values.

```json
{
  "name": "custom-server",
  "type": "stdio",
  "cmd": ["python3", "/opt/servers/custom/main.py"],
  "env": {
    "LOG_LEVEL": "debug",
    "API_KEY": "sk-12345..."
  }
}
```

### `token` (optional)
Authentication token for the server. Control characters are stripped during materialization.

```json
{
  "name": "authenticated-server",
  "type": "http",
  "url": "https://api.example.com/mcp",
  "token": "Bearer xyz-token-123"
}
```

## Injection Paths

### Top-Level `mcp_servers` Array (Recommended)

The `mcp_servers` array in the session request is the recommended approach for configuring MCP servers. It is agent-agnostic and applies to both Claude and Codex agents.

```json
{
  "agent": "claude",
  "mcp_servers": [
    {
      "name": "filesystem",
      "type": "stdio",
      "cmd": ["npx", "@modelcontextprotocol/server-filesystem", "/workspace"]
    },
    {
      "name": "web-server",
      "type": "http",
      "url": "http://localhost:3000"
    }
  ]
}
```

### Claude-Specific `claude.mcp_json`

For advanced scenarios, you can provide raw `.mcp.json` content via the `claude.mcp_json` field. This is merged with `mcp_servers` at materialization time.

```json
{
  "agent": "claude",
  "claude": {
    "mcp_json": {
      "mcpServers": {
        "legacy-server": {
          "type": "http",
          "url": "http://api.example.com"
        }
      }
    }
  },
  "mcp_servers": [
    {
      "name": "new-server",
      "type": "stdio",
      "cmd": ["node", "server.js"]
    }
  ]
}
```

When both are present, entries in `mcp_servers` take precedence. The final `.mcp.json` written to the agent's home directory contains all servers from both sources.

## ${HOST_GATEWAY} Resolution

When running the agent in a Docker container, servers running on the host machine are unreachable via `localhost` or `127.0.0.1`. Use the `${HOST_GATEWAY}` placeholder in URLs to reference the Docker host gateway address.

At materialization time, `${HOST_GATEWAY}` is replaced with:

- **macOS**: `host.docker.internal`
- **Linux**: The default route gateway (e.g., `172.17.0.1`), falling back to `172.17.0.1` if unresolvable
- **Other**: `host.docker.internal`

### Example: Local HTTP Server Accessible from Container

**Before materialization** (session request):
```json
{
  "name": "local-api",
  "type": "http",
  "url": "http://${HOST_GATEWAY}:8080/mcp"
}
```

**After materialization** (in agent's `.mcp.json`, on macOS):
```json
{
  "type": "http",
  "url": "http://host.docker.internal:8080/mcp"
}
```

**After materialization** (in agent's `.mcp.json`, on Linux):
```json
{
  "type": "http",
  "url": "http://172.17.0.1:8080/mcp"
}
```

This allows a container to reach a service listening on `127.0.0.1:8080` on the host.

## URL Sanitization

The materialization process validates and sanitizes URLs to prevent configuration errors:

- Only schemes `http`, `https`, `ws`, and `wss` are allowed.
- Unsupported schemes (e.g., `ftp://`, `file://`, custom protocols) are silently removed, resulting in an empty URL field.
- If a URL becomes empty after sanitization, it is dropped from the final configuration.
- Scheme matching is case-insensitive.

### Examples

**Valid:**
- `http://localhost:3000`
- `https://api.example.com/mcp`
- `ws://localhost:8000`
- `wss://secure.example.com`

**Invalid (dropped):**
- `ftp://example.com` → removed
- `file:///etc/config` → removed
- `ssh://host` → removed

## Token Sanitization

Authentication tokens are sanitized to remove control characters (ASCII 0x00–0x1F and 0x7F). This prevents injection of newlines or other problematic characters into the configuration.

Example:
```
Raw token:    "xyz\x00abc\ntoken"
Sanitized:    "xyzabctoken"
```

All printable characters and high-bit characters are preserved.

## Complete Examples

### HTTP MCP Server

```json
{
  "name": "example-http",
  "type": "http",
  "url": "http://localhost:3000",
  "token": "Bearer sk-token123"
}
```

Session request:
```json
{
  "agent": "claude",
  "mcp_servers": [
    {
      "name": "example-http",
      "type": "http",
      "url": "http://localhost:3000",
      "token": "Bearer sk-token123"
    }
  ]
}
```

Resulting `.mcp.json`:
```json
{
  "mcpServers": {
    "example-http": {
      "type": "http",
      "url": "http://localhost:3000",
      "token": "Bearer sk-token123"
    }
  }
}
```

### Stdio MCP Server

```json
{
  "name": "filesystem",
  "type": "stdio",
  "cmd": ["npx", "@modelcontextprotocol/server-filesystem", "/workspace"]
}
```

Session request:
```json
{
  "agent": "claude",
  "mcp_servers": [
    {
      "name": "filesystem",
      "type": "stdio",
      "cmd": ["npx", "@modelcontextprotocol/server-filesystem", "/workspace"]
    }
  ]
}
```

Resulting `.mcp.json`:
```json
{
  "mcpServers": {
    "filesystem": {
      "type": "stdio",
      "cmd": ["npx", "@modelcontextprotocol/server-filesystem", "/workspace"]
    }
  }
}
```

### Server with Authentication

```json
{
  "name": "secure-api",
  "type": "http",
  "url": "https://api.example.com/mcp",
  "token": "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
}
```

Session request:
```json
{
  "agent": "claude",
  "mcp_servers": [
    {
      "name": "secure-api",
      "type": "http",
      "url": "https://api.example.com/mcp",
      "token": "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
    }
  ]
}
```

### Server Accessible from Container via HOST_GATEWAY

A service running on the host at `http://127.0.0.1:8080/tools` should be configured with:

```json
{
  "name": "host-tools",
  "type": "http",
  "url": "http://${HOST_GATEWAY}:8080/tools"
}
```

Session request:
```json
{
  "agent": "claude",
  "mcp_servers": [
    {
      "name": "host-tools",
      "type": "http",
      "url": "http://${HOST_GATEWAY}:8080/tools"
    }
  ]
}
```

At materialization:
- **macOS**: `http://host.docker.internal:8080/tools`
- **Linux**: `http://172.17.0.1:8080/tools` (or detected gateway)

### Multiple Servers

Session request:
```json
{
  "agent": "claude",
  "mcp_servers": [
    {
      "name": "filesystem",
      "type": "stdio",
      "cmd": ["npx", "@modelcontextprotocol/server-filesystem", "/workspace"]
    },
    {
      "name": "postgres",
      "type": "http",
      "url": "http://${HOST_GATEWAY}:5432",
      "token": "postgres-token"
    },
    {
      "name": "local-tools",
      "type": "stdio",
      "cmd": ["python3", "/opt/tools/main.py"],
      "env": {
        "DEBUG": "1"
      }
    }
  ]
}
```

Resulting `.mcp.json`:
```json
{
  "mcpServers": {
    "filesystem": {
      "type": "stdio",
      "cmd": ["npx", "@modelcontextprotocol/server-filesystem", "/workspace"]
    },
    "postgres": {
      "type": "http",
      "url": "http://172.17.0.1:5432",
      "token": "postgres-token"
    },
    "local-tools": {
      "type": "stdio",
      "cmd": ["python3", "/opt/tools/main.py"],
      "env": {
        "DEBUG": "1"
      }
    }
  }
}
```
