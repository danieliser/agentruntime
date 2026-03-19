# Skills, Agents, and Plugins in agentruntime

This guide covers how to configure Claude Code skills, agents, and plugins within agentruntime sessions. It also addresses the equivalent patterns for Codex.

## Claude Code: CLAUDE.md Injection

The primary mechanism for extending Claude Code's behavior is injecting a `CLAUDE.md` file containing custom instructions, context, and persona configuration.

### How It Works

When you provide the `claude_md` field in a `SessionRequest`, agentruntime writes the content to `~/.claude/CLAUDE.md` inside the agent's container. Claude Code reads this file on startup and applies the instructions to its behavior.

### Example: Project-Specific Instructions

This example configures Claude Code to enforce specific coding standards for a TypeScript project:

```yaml
agent: claude
prompt: "Add type safety to the auth module"
work_dir: /path/to/my/typescript/project
claude:
  claude_md: |
    # My Project Standards

    ## TypeScript Guidelines
    - Always use strict mode and enable `strict: true` in tsconfig.json
    - Export types explicitly with `export type`, not `type MyType = ...`
    - Avoid `any` — use `unknown` and type narrowing instead
    - Async functions must always return a Promise

    ## Code Style
    - Use 2-space indentation
    - Single quotes for strings
    - Trailing commas in multi-line objects/arrays
    - Max line length: 100 characters

    ## Testing
    - Write unit tests for every utility function
    - Use Jest for test runner
    - Coverage target: 80% minimum
```

### CLAUDE.md Contents Reference

The file supports full Markdown and can include:

- **Project context** — Architecture, conventions, tech stack decisions
- **Instructions** — How to approach tasks, what to prioritize
- **Persona** — Voice, tone, communication style
- **Constraints** — Tools to avoid, patterns not allowed, frameworks required
- **References** — Links to docs, examples, or standards

See the official Claude Code documentation for detailed syntax and best practices.

## MCP Servers as Tool Providers

Claude Code plugins and skills often depend on MCP (Model Context Protocol) servers to provide specialized tools. agentruntime injects MCP servers via the `mcp_servers` array in the `SessionRequest`.

### MCPServer Configuration

Each server in the array specifies:

- **name** (required) — identifier for the server, e.g. `"my-filesystem-tool"`
- **type** (required) — communication protocol: `"http"`, `"stdio"`, or `"websocket"`
- **url** — for HTTP/WebSocket types, the endpoint URL (supports `${HOST_GATEWAY}` substitution in Docker mode)
- **cmd** — for stdio type, the command array to spawn the server process
- **env** — optional environment variables passed to the server
- **token** — optional authentication token

### Example: Custom Filesystem Tool via stdio

This example configures a custom MCP server that provides safe filesystem operations:

```yaml
agent: claude
prompt: "Scan and report on all shell scripts in the project"
work_dir: /path/to/my/project
mcp_servers:
  - name: "filesystem-scanner"
    type: "stdio"
    cmd: ["node", "/opt/mcp-servers/filesystem-scanner.js"]
    env:
      MAX_DEPTH: "5"
      IGNORED_PATTERNS: ".git,node_modules,dist"
```

When the sidecar materializes the session, it writes these servers to the agent's `.mcp.json` config file. Claude Code then loads the servers and makes their tools available during the session.

### Example: HTTP Tool Server

If you have a tool server already running on the host (or in a sidecar container):

```yaml
agent: claude
prompt: "Check API health and generate a report"
work_dir: /path/to/my/project
mcp_servers:
  - name: "api-health-check"
    type: "http"
    url: "http://${HOST_GATEWAY}:8888"
    token: "sk-secret-key-here"
```

In Docker mode, `${HOST_GATEWAY}` is automatically resolved to the host's IP address so the agent can reach services on the host.

### MCP JSON Merging

If you also provide `claude.mcp_json` in the request, agentruntime merges it with the `mcp_servers` array:

```yaml
claude:
  mcp_json:
    mcpServers:
      # This server is defined in inline JSON
      my-custom-tool:
        type: stdio
        cmd: ["python3", "/custom/tool.py"]
  # And these servers are added from the array
mcp_servers:
  - name: "another-tool"
    type: "http"
    url: "http://localhost:9000"
```

The final `.mcp.json` contains both sources.

## Custom Agents via Volume Mount

Claude Code supports `.claude/agents/` directory for custom subagent definitions. These are agent configurations you create and want Claude to discover.

If your host has agent definitions in a directory, you can mount them into the container:

```yaml
agent: claude
prompt: "Use the custom research-agent to investigate this bug"
work_dir: /path/to/my/project
mounts:
  - host: /path/to/custom-agents
    container: /home/agent/.claude/agents
    mode: ro
```

Now Claude Code will find your custom agents in the `.claude/agents` directory and can delegate tasks to them using the agent selection interface.

## Limitations

### Claude Code Plugins

Plugins installed via `/plugin` in Claude Code are **not automatically available** in containers. To use plugins:

- **Bake them into the image**: Extend `agentruntime-agent:latest` with a Dockerfile that installs desired plugins
- **Provide plugin code via mount**: If the plugin is open source, mount its directory to the expected plugin path

Most first-party plugins (GitHub, Slack, Notion, etc.) require authentication tokens that must be configured in the container's environment or mounted as credential files.

### Filesystem-Dependent Skills

Skills that rely on local filesystem state (e.g., checking for IDE extensions, reading the user's dotfiles) may not work in Docker mode because the container has minimal filesystem context. Prefer MCP servers for shared behavior that works across environments.

### Sidecar Lifecycle

The sidecar controls the agent process lifecycle. Plugins that modify startup behavior, hook into the CLI bootstrap, or rely on shell integration may conflict with the sidecar's environment setup. Test such plugins carefully in your target runtime.

## Codex Equivalent

Codex uses a different configuration model:

- **Instructions** — Use `codex.instructions` field (instead of `claude_md`) for custom instructions
- **No plugin/skill system** — Codex does not have a plugins mechanism
- **Custom tools** — Add tools via Codex's native tool system in `codex.config_toml`

Example:

```yaml
agent: codex
prompt: "Refactor this Python module"
work_dir: /path/to/project
codex:
  instructions: |
    # Python Coding Standards
    - Use Python 3.10+ features (match statements, union types)
    - Type hints are mandatory
    - Follow PEP 8 with black formatter
```

Codex does not support MCP servers; custom tools must be defined within Codex's configuration.

## Complete Example: Claude with Skills and MCP

Here's a full example combining CLAUDE.md instructions, MCP tool servers, and settings:

```yaml
agent: claude
prompt: "Implement a web scraper for the given URL and save results to database"
work_dir: /path/to/my/project

claude:
  claude_md: |
    # Web Scraper Project

    ## Architecture
    - Use Playwright for browser automation
    - Data persists to PostgreSQL via SQLAlchemy ORM
    - All I/O is async with asyncio

    ## Code Standards
    - Python 3.11+, type hints required
    - Format with black (line length 100)
    - Lint with ruff
    - Test with pytest

    ## Tools Available
    - `web-scraper-mcp` — fetch and parse HTML with retries
    - `db-query-mcp` — safe SQL execution with prepared statements

  settings_json:
    skipDangerousModePermissionPrompt: true
    # Allow Claude to use the web scraper and database tools
    permissions:
      mcp:
        - web-scraper-mcp
        - db-query-mcp

mcp_servers:
  - name: "web-scraper-mcp"
    type: "stdio"
    cmd: ["python3", "/opt/tools/web-scraper/server.py"]
    env:
      USER_AGENT: "Mozilla/5.0 (X11; Linux x86_64)"
      TIMEOUT_SECONDS: "30"

  - name: "db-query-mcp"
    type: "http"
    url: "http://${HOST_GATEWAY}:8765"
    token: "${DB_MCP_TOKEN}"

container:
  memory: "2g"
  cpus: 1.5

env:
  DB_MCP_TOKEN: "sk-1234567890abcdef"
  DATABASE_URL: "postgresql://user:pass@localhost/mydb"
```

When this request runs:

1. **CLAUDE.md is written** to `~/.claude/CLAUDE.md` with project standards and tool guidance
2. **MCP servers are materialized** into `.mcp.json` with both stdio and HTTP configurations
3. **Settings are applied**, pre-approving dangerous mode and tool permissions
4. **Environment variables** are passed to the container for tool authentication
5. **Container is allocated** 2GB memory and 1.5 CPUs for the resource-intensive scraping task
6. Claude Code starts in `/workspace` with access to all tools and instructions

The sidecar normalizes all tool calls and results back through the event stream, allowing you to observe and steer the agent's behavior in real time.
