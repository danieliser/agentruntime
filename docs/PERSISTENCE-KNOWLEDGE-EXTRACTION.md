# Knowledge Extraction: Persistence Executor Layer

Extracted from the PAOP/Persistence codebase before the executor layer teardown.
These are battle-tested patterns, configurations, and solutions that should inform
future agentruntime features.

---

## 1. Tmux Session Management (Future Feature Candidate)

### Working tmux Spawn Pattern

The persistence codebase had a fully working tmux integration. Key parameters:

```bash
tmux new-session -d \
  -s "paop-{task_id[:8]}" \   # Named session for discovery
  -x 200 -y 50 \              # Fixed geometry (avoids resize noise)
  -c /workspace \              # Working directory
  "env -i /bin/bash -c '...'"  # Clean-room environment
```

**Critical settings applied post-spawn:**

```bash
# Keep pane alive after process exits — user can still view scrollback
tmux set-option -p -t {session_name} remain-on-exit on

# pipe-pane for logging (preserves TTY, unlike tee)
tmux pipe-pane -t {session_name} -o "cat >> {log_path}"
```

### Tmux Session Lifecycle

1. **Spawn**: `tmux new-session -d` with fixed 200x50 geometry
2. **Logging**: Two approaches used:
   - **Batch mode**: `agent_cmd 2>&1 | tee {log_path}` (simple, loses TTY)
   - **Interactive mode**: `tmux pipe-pane -o "cat >> {log_path}"` (preserves TTY)
   - **Timestamped logging**: `perl -pe 'use POSIX qw(strftime); $| = 1; print strftime("[%Y-%m-%dT%H:%M:%S] ", localtime)'` piped before tee
3. **remain-on-exit**: Set at pane level (`-p` flag) so scrollback survives process exit
4. **Cleanup**: `tmux kill-session -t {session_name}` with error swallowing

### Tmux Liveness Detection

```python
# Check if pane process has exited (remain-on-exit keeps session alive)
tmux list-panes -t {session_name} -F "#{pane_dead}"
# Returns "1" if process exited, "0" if still running

# Check if session exists at all
tmux has-session -t {session_name}

# Check what command is running in the pane
tmux display-message -t {session_name} -p "#{pane_current_command}"
# Used to verify an agent (not just a shell) is running
```

### Tmux Streaming Detection (Anti-Injection Guard)

Before sending messages to a running agent, check if the pane output is actively changing:

```python
async def _is_pane_streaming(session_name, sample_interval=0.5):
    snapshot1 = capture_pane(session_name, last_5_lines=True)
    await asyncio.sleep(sample_interval)
    snapshot2 = capture_pane(session_name, last_5_lines=True)
    return snapshot1 != snapshot2  # If different, agent is mid-response
```

This prevents injecting prompts while an agent is actively writing output —
which can corrupt the response or cause the agent to misinterpret the injection.

The `force=True` parameter bypasses this for interactive sessions where the
Claude Code TUI renders continuously (always looks "active").

### Tmux Message Delivery (send-keys)

Two-step Enter pattern for reliable prompt delivery:

```python
# Step 1: Send the message text + Enter
tmux send-keys -t {session_name} {message} Enter

# Step 2: Small delay, then bare Enter to confirm
await asyncio.sleep(0.1)
tmux send-keys -t {session_name} Enter
```

For Docker containers, same pattern but via `docker exec`:

```bash
docker exec {container_id} tmux send-keys -t {session_name} {message} Enter
```

### Tmux Session Environment Variables

Session-scoped env vars stored via tmux's own mechanism:

```python
# Store env file path in tmux session metadata
tmux set-environment -t {session_name} PAOP_SESSION_ENV_FILE {path}

# Retrieve it later for cleanup
tmux show-environment -t {session_name} PAOP_SESSION_ENV_FILE
```

### Tmux iTerm Integration

AppleScript to open tmux session in a new iTerm tab (no TTY required):

```applescript
tell application "iTerm"
    activate
    tell current window
        create tab with default profile command \
            "{tmux_path} attach -t {session_name}"
    end tell
end tell
```

For Docker containers:

```applescript
create tab with default profile command \
    "{docker_path} exec -it -e TERM=xterm-256color {container_name} \
     {tmux_path} attach-session -t {session_name}"
```

Note: Must resolve full tmux path (`shutil.which("tmux")` or `/opt/homebrew/bin/tmux`)
since iTerm profile commands don't search $PATH.

### Tmux Prompt Marker Detection

Wait for an agent's prompt to appear before sending input:

```python
# Agent prompt markers (regex)
CLAUDE_CODE_PROMPT = r"[❯>]\s*$"
CODEX_PROMPT = r"[›>]\s*$"

# Poll capture-pane until marker appears in last 5 lines
while elapsed < timeout:
    lines = tmux_capture_pane(session_name)
    for line in reversed(lines[-5:]):
        if re.search(prompt_marker, line.rstrip()):
            return True  # Agent is ready for input
    await asyncio.sleep(0.5)
```

---

## 2. Docker Container Configuration

### Container Security Hardening

```python
args = [
    "--detach",
    "--name", f"paop-{task_id[:8]}",
    "--network", "paop-agents",             # Isolated bridge network
    "--user", f"{host_uid}:{host_gid}",     # UID/GID matching
    "--workdir", "/workspace",
    "--cap-drop", "ALL",                     # Drop all capabilities
    "--cap-add", "DAC_OVERRIDE",             # Only add what's needed
    "--security-opt", "no-new-privileges:true",
    "--memory", "2g",                        # Memory limit
    "--cpu-shares", "512",                   # CPU weight
    "--init",                                # tini for signal handling
]
```

If not persistent mode, add `--rm` for auto-cleanup.

### Volume Mounts (Complete Map)

```python
# Workspace (project files)
"--volume", f"{cwd}:/workspace:rw"

# Log directory
"--volume", f"{host_log_dir}:{container_log_dir}:rw"

# PAOP platform source (for MCP server inside container)
"--volume", f"{paop_source}:/paop-src:ro"

# Claude config directory (from agent-home, not personal ~/.claude)
"--volume", f"{agent_claude_dir}:/home/appuser/.claude:rw"

# Persistent session volume (named Docker volume for session continuity)
"--volume", f"paop-session-{session_id}:/home/appuser/.claude/projects"

# Claude account state
"--volume", f"{host_claude_json}:/home/appuser/.claude.json:rw"

# OAuth credentials (synced from Keychain pre-spawn)
"--volume", f"{creds_path}:/home/appuser/.claude/.credentials.json:ro"

# Codex auth (conditional on agent type)
"--volume", f"{host_codex_dir}:/home/appuser/.codex:rw"

# Per-task agent config overlays (settings.json, .mcp.json, CLAUDE.md, AGENTS.md)
"--volume", f"{staging_dir/.claude/settings.json}:/home/appuser/.claude/settings.json:ro"
"--volume", f"{staging_dir/.mcp.json}:/workspace/.mcp.json:ro"
"--volume", f"{staging_dir/CLAUDE.md}:/workspace/CLAUDE.md:ro"
"--volume", f"{staging_dir/AGENTS.md}:/workspace/AGENTS.md:ro"
```

### Container Environment Variables

**Non-sensitive (via --env):**

```python
env = {
    "HOME": "/home/appuser",
    "PATH": "/usr/local/bin:/usr/bin:/bin",
    # Egress proxy
    "HTTP_PROXY": f"http://paop-proxy:3128",
    "HTTPS_PROXY": f"http://paop-proxy:3128",
    "http_proxy": f"http://paop-proxy:3128",
    "https_proxy": f"http://paop-proxy:3128",
    "NO_PROXY": "localhost,127.0.0.1,host.docker.internal,host-gateway",
    "no_proxy": "localhost,127.0.0.1,host.docker.internal,host-gateway",
    # Internal metadata
    "PAOP_TASK_ID": task_id,
    "PAOP_AGENT_NAME": agent_name,
    "PAOP_ENDPOINT": endpoint_url,
}
```

**Sensitive (via --env-file, temp file 0o600):**

```python
_API_KEY_ENV_VARS = frozenset({
    "CLAUDE_CODE_OAUTH_TOKEN",
    "GH_TOKEN",
    "GITHUB_TOKEN",
})
# Written to tempfile with mkstemp, chmod 0o600
# Deleted immediately after docker run completes
```

**Explicitly excluded:**

```python
# AI provider keys NEVER forwarded — agents use OAuth
for key in ("ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY"):
    env.pop(key, None)
# Host-only vars removed
for key in ("TMPDIR", "XDG_CONFIG_HOME", "SHELL"):
    env.pop(key, None)
```

### Host Gateway Resolution (macOS vs Linux)

```python
if platform.system() == "Darwin":
    # Docker Desktop: host.docker.internal resolves to host
    paop_endpoint = "http://host.docker.internal:8801"
    extra_host_args = []
else:
    # Linux: must add host-gateway manually
    paop_endpoint = "http://host-gateway:8801"
    extra_host_args = ["--add-host", "host-gateway:host-gateway"]
```

### Docker Runtime Detection (OrbStack + Docker Desktop)

```python
# Detection order:
# 1. Check OrbStack socket: ~/.orbstack/run/docker.sock
# 2. Check OrbStack context: `docker context ls` contains "orbstack"
# 3. Fall back to Docker Desktop: `docker info` succeeds

# Detection is cached permanently on success.
# On failure, retries every 30 seconds.
# Uses 5-second timeouts on all Docker CLI calls.
```

### Linux Bridge Interface Validation

```python
# Verify br-paop bridge exists
os.path.exists("/sys/class/net/br-paop")

# Linux gateway resolution from routing table
with open("/proc/net/route") as handle:
    # Parse hex gateway from default route (0x00000000 destination)
    gateway_hex = fields[2]  # e.g. "0101A8C0" -> 192.168.1.1
    gateway = bytes.fromhex(gateway_hex)[::-1]  # Reverse byte order
    ip = ".".join(map(str, gateway))
```

---

## 3. Network Isolation

### Bridge Network Setup

```bash
docker network create \
  --driver bridge \
  --opt com.docker.network.bridge.name=br-paop \
  paop-agents
```

### Linux iptables Rules (Host Isolation)

Prevents inter-container lateral movement while keeping daemon access:

```bash
# Check if rule exists (idempotent)
iptables -C DOCKER-USER \
  -i br-paop ! -o br-paop \
  -p tcp ! --dport 8801 \
  -j DROP

# Insert if missing
iptables -I DOCKER-USER \
  -i br-paop ! -o br-paop \
  -p tcp ! --dport 8801 \
  -j DROP
```

Rule meaning: Drop all TCP traffic FROM the bridge network TO non-bridge
destinations, EXCEPT traffic to port 8801 (daemon API).

### Squid Egress Proxy Sidecar

```python
PROXY_CONTAINER_NAME = "paop-proxy"
PROXY_IMAGE = "paop-proxy:latest"
PROXY_PORT = 3128

docker run --detach \
  --name paop-proxy \
  --network paop-agents \
  --memory 256m \
  --cpu-shares 128 \
  paop-proxy:latest
```

Agent containers set `HTTP_PROXY`/`HTTPS_PROXY` to `http://paop-proxy:3128`.
Squid filters outbound connections by domain allowlist (squid.conf) at the
CONNECT level — no SSL bump, just tunnel allow/deny by destination hostname.

---

## 4. Credential Management

### macOS Keychain → Docker Credential Sync

Claude Code v2.1+ stores OAuth tokens in macOS Keychain under
`"Claude Code-credentials"`. Docker containers can't access Keychain.

**Sync flow:**

1. Check Keychain modification date via `security find-generic-password -s "Claude Code-credentials"` (parse `mdat` attribute)
2. Compare against cached file mtime — skip extraction if unchanged
3. Extract credentials: `security find-generic-password -s "Claude Code-credentials" -w` (returns JSON)
4. Write atomically to `~/.local/share/paop/credentials/claude-credentials.json` (0o600)
5. Also refresh `~/.claude/.credentials.json` for host CLI agents
6. Throttle checks to every 30 seconds

**OAuth token extraction:**

```python
with open(creds_path) as f:
    data = json.load(f)
    token = data["claudeAiOauth"]["accessToken"]
```

### Clean-Room Environment for Agents

Two tiers of environment building:

**System passthrough (local agents):**

```python
SYSTEM_ENV_PASSTHROUGH = {"PATH", "HOME", "SHELL", "TERM", "LANG", "USER", "TMPDIR"}
```

**CLI env allowlist (Docker containers):**

```python
CLI_ENV_ALLOWLIST = {"PATH", "HOME", "USER", "SHELL", "LANG", "LC_ALL", "TERM", "TMPDIR"}
```

Both ensure `~/.local/bin` is prepended to PATH (where `claude` CLI lives).

---

## 5. Agent Registry & Argument Building

### Agent Definitions

```python
CLAUDE_CODE = AgentDefinition(
    agent_id="claude-code",
    command="claude",
    base_args=["--print", "--verbose", "--output-format", "stream-json",
               "--dangerously-skip-permissions"],
    resume_args=lambda sid: ["--resume", sid],
    readonly_args=["--permission-mode", "plan"],
    model_args=lambda m: ["--model", m],
    effort_args=lambda e: ["--effort", e],
    prompt_args=lambda p: [p],  # Positional
    interactive_args=["--dangerously-skip-permissions"],
    prompt_marker=r"[❯>]\s*$",
    env_unset=["CLAUDECODE"],
    session_id_args=lambda sid: ["--session-id", sid],
    session_log_dir="~/.claude/projects",
)

CODEX = AgentDefinition(
    agent_id="codex",
    command="codex",
    batch_mode_prefix=["exec"],
    batch_mode_args=["--dangerously-bypass-approvals-and-sandbox",
                     "--skip-git-repo-check"],
    json_output_args=["--json"],
    resume_args=lambda sid: ["resume", sid],
    readonly_args=["--sandbox", "read-only"],
    model_args=lambda m: ["--model", m],
    prompt_args=lambda p: [p],
    interactive_args=["--full-auto"],
    prompt_marker=r"[›>]\s*$",
    session_id_args=None,  # Codex generates internally
    session_log_dir="~/.codex/sessions",
    output_file_args=lambda path: ["-o", path],
)
```

### Hierarchical Argument Construction

Layer order (batch mode):

1. Batch mode prefix (e.g., `codex exec`)
2. Base args
3. Batch mode args
4. JSON output args
5. Output file args (`-o`)
6. Read-only mode
7. Model selection
8. Effort level
9. Session resumption (`--resume`)
10. New session ID (`--session-id`)
11. Custom overrides
12. Prompt (unless stdin delivery)

Interactive mode uses different args entirely — no batch prefix, no JSON output,
prompt delivered via send-keys.

---

## 6. Stall Detection & Polling

### Two-Phase Stall Detection

```python
# Phase 1: Advisory warning at stall_timeout (default 600s = 10 min)
# Just logs — doesn't kill the agent
if quiet_seconds > stall_timeout:
    logger.info("Agent quiet for %ds (still alive)", quiet_seconds)

# Phase 2: Hard kill at 5x stall_timeout (default 3000s = 50 min)
# Raises AgentStallError for retry with backoff
if quiet_seconds > stall_timeout * 5:
    raise AgentStallError(session_name, quiet_seconds)
```

### Result Marker Detection

Early exit from polling when the agent emits its completion event:

```python
_RESULT_MARKERS = [b'"type":"result"', b'"type":"turn.completed"']
_RESULT_TAIL_BYTES = 32 * 1024  # Only scan last 32KB

def _log_contains_result(log_path):
    with open(log_path, "rb") as f:
        f.seek(0, 2)
        size = f.tell()
        f.seek(max(0, size - _RESULT_TAIL_BYTES))
        tail = f.read()
    return any(marker in tail for marker in _RESULT_MARKERS)
```

If result marker detected but pane isn't dead yet, allow 2 poll cycle grace period
then break — agent finished but its process tree hung (MCP servers not shutting down).

### Persistent Container Polling

For persistent/interactive sessions, uses a completion marker pattern:

```python
completion_marker = f"__PAOP_DONE__:{uuid4().hex}"
full_cmd = f"{agent_cmd}; printf '%s\\n' {marker} >> {log_path}"
# Poll log for the marker string, separate from container liveness
```

### Lock Renewal During Long Tasks

```python
# Worktree locks renewed periodically to prevent stale cleanup
if lock_renewal_callback and (now - last_lock_renewal) >= 300.0:
    await lock_renewal_callback()  # Best-effort — don't kill task on failure
```

---

## 7. Output Parsing & Error Detection

### Error Pattern Registry

```python
_ERROR_PATTERNS = [
    (r"There's an issue with the selected model", "model_not_found"),
    (r"model.*(?:not exist|not found|not available|no access)", "model_not_found"),
    (r"invalid.*model", "model_not_found"),
    (r"(?:authentication|auth).*(?:failed|error|invalid)", "auth_error"),
    (r"API key.*(?:invalid|missing|expired)", "auth_error"),
    (r"permission.*denied", "permission_denied"),
    (r"rate.*limit.*exceeded", "rate_limit"),
    (r"duplicate session", "duplicate_session"),
    (r"session.*already exists", "duplicate_session"),
    (r"API Error:\s*\d{3}", "upstream_api_error"),
    (r'"type"\s*:\s*"error".*"Internal server error"', "upstream_api_error"),
    (r"(?:overloaded|529|503).*(?:error|try again)", "upstream_api_error"),
]
```

### Codex JSONL False Positive Prevention

Codex JSONL embeds MCP results and tool outputs in raw JSON lines. Scanning
those for error patterns causes false positives (e.g., `"authorization"..."error":null`
in embedded content). Solution: extract only `agent_message` text and top-level
`error` events for pattern matching — never raw JSONL lines.

### Zero-Token Startup Crash Detection

```python
# Zero usage with short output = likely crashed before doing work
if input_tokens == 0 and output_tokens == 0 and len(log_content) < 2000:
    return True, "startup_crash"
# Skipped for Codex — token usage comes from turn.completed events
```

---

## 8. Agent Home (Controlled Config Directory)

### Bootstrap Pattern

Creates `~/.local/share/paop/agent-home/claude/` with:

```python
settings = {
    "permissions": {
        "allow": [
            "Read", "Read(*)", "Edit", "Write", "MultiEdit",
            "Bash(git:*)", "Bash(ls:*)", "Bash(find:*)", "Bash(grep:*)",
            "Bash(cat:*)", "Bash(mkdir:*)", "Bash(cp:*)", "Bash(mv:*)",
            "Bash(python3:*)", "Bash(python:*)", "Bash(uv:*)",
            "Bash(npm:*)", "Bash(pnpm:*)", "Bash(node:*)",
            "Bash(jq:*)", "Bash(yq:*)", "Bash(tree:*)", "Bash(sqlite3:*)",
            "WebSearch", "WebFetch",
            "mcp__persist__*",
        ],
        "deny": [],
    },
    "hooks": {},           # No hooks — agents must not load personal hook scripts
    "telemetry": {"enabled": False},
    "cleanupPeriodDays": 1,
    "env": {"CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR": "true"},
}
```

Set as `XDG_CONFIG_HOME` for agent sessions so Claude reads this controlled
config instead of the operator's personal `~/.claude/`.

### Session Env File Pattern

Write a shell fragment for sourcing inside tmux:

```python
# Write to agent-home/.session-env with 0o600 permissions
export PAOP_TASK_ID='abc123'
export PAOP_AGENT_NAME='agent-name'
export PAOP_ENDPOINT='http://localhost:8801'
export PERSIST_TOOL_TIER='1'
export XDG_CONFIG_HOME='/path/to/agent-home'

# Source in tmux command:
inner_cmd = f"source {env_path} && {agent_cmd}"
shell_cmd = f"env -i /bin/bash -c '{inner_cmd}'"
```

---

## 9. Project Root Security Validation

```python
SENSITIVE_DIRS = frozenset({
    ".ssh", ".gnupg", ".aws", ".config/gcloud",
    ".kube", ".docker", "Library/Keychains",
})

# Checks:
# 1. Path must be absolute
# 2. Resolve symlinks to detect traversal
# 3. Must not contain sensitive directories (relative to $HOME)
# 4. Must not be filesystem root
# 5. Must exist and be a directory
# 6. No ".." in original path components
```

---

## 10. Persistent Docker Sessions

### Named Volumes for Session Continuity

```python
# Volume name convention
volume_name = f"paop-session-{session_id}"

# Mount at Claude's project cache
"--volume", f"{volume_name}:/home/appuser/.claude/projects"

# Create volume explicitly
docker volume create {volume_name}

# Remove on deregistration
docker volume rm {volume_name}
```

### Session Continuation in Running Containers

For persistent containers (interactive/session-continuation modes):

- Container is NOT `--rm` (stays alive between commands)
- tmux session created empty (`tmux new-session -d -s {name}`)
- Container main process: `while tmux has-session -t {name} 2>/dev/null; do sleep 2; done`
- Agent commands injected via `tmux send-keys` into the running session
- Completion detected via marker string appended to log file

### Orphaned Container Recovery

On daemon restart:

1. Query DB for tasks with `execution_mode='docker'` and `status='running'`
2. Check if container is still running: `docker inspect --format "{{.State.Running}}"`
3. Container alive → log warning (result lost, need re-attach in v2)
4. Container gone + log exists → salvage result from bind-mounted log file
5. Container gone + no log → reset task to 'pending' for retry

---

## 11. WebSocket Output Forwarding

### Log File Tailing → WebSocket Broadcast

```python
# Background asyncio task tails the log file and broadcasts new bytes
async def _forward_session_output(session_name, log_path, poll_interval=0.25):
    last_offset = 0
    while True:
        current_size = os.path.getsize(log_path)
        if current_size > last_offset:
            with open(log_path, "rb") as handle:
                handle.seek(last_offset)
                chunk = handle.read()
            await subscription_manager.broadcast(WSEvent(
                event_type="session_output",
                channel=f"session:{session_name}",
                data={"session_name": ..., "chunk": ..., "total_bytes": ...},
            ))
            last_offset = current_size

        # Stop when pane is dead and no more data
        if pane_dead and current_size <= last_offset:
            dead_polls += 1
            if dead_polls >= 2:
                return
        await asyncio.sleep(poll_interval)
```

---

## 12. Adaptive Message Delivery

### Multi-Channel Routing

```python
# Delivery priority:
# 1. tmux send-keys (for cli/tmux sessions) — SLA target: 100ms
# 2. docker exec + tmux send-keys (for Docker sessions)
# 3. agentruntime WS stdin (for agentruntime sessions)
# 4. Polling queue (fallback for all) — SLA target: 60s

# Each channel has latency tracking (min/max/avg/success rate)
```

---

## Summary: What agentruntime Should Eventually Support

1. **Tmux runtime mode**: Alternative to the sidecar WS approach, useful for
   local development, debugging, and interactive sessions where you want to
   `tmux attach` directly. Key primitives: spawn with fixed geometry, pipe-pane
   logging, remain-on-exit, streaming detection guard, prompt marker detection.

2. **Egress proxy sidecar**: Squid-based domain allowlist filtering. Currently
   in agentruntime's Docker runtime already, but the configuration patterns
   above are battle-tested.

3. **Credential sync from macOS Keychain**: The Keychain → file sync is needed
   for any Docker-based execution. The throttled, mtime-compared approach avoids
   unnecessary Keychain access.

4. **Agent argument registry**: The declarative AgentDefinition pattern with
   hierarchical argument construction is worth considering if agentruntime
   ever needs to support more than claude/codex.

5. **Stall detection**: Two-phase (advisory → hard kill) with result marker
   early exit. The 5x multiplier on stall timeout before hard kill is a
   good balance.

6. **Output error classification**: The regex-based error pattern registry
   with special Codex JSONL handling prevents false positives.

7. **Session persistence via named Docker volumes**: Clean pattern for
   session continuity across container restarts.

8. **Project root security validation**: The sensitive directory blocklist
   and traversal detection should be in agentruntime's validation.
