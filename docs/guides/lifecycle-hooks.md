# Container Lifecycle Hooks

Lifecycle hooks are scripts that run at defined points in a session's container lifecycle. They enable workspace setup, agent warmup, cost monitoring, artifact extraction, and security enforcement — without modifying agent binaries.

## Quick Start

```bash
curl -X POST http://localhost:8090/sessions \
  -H 'Content-Type: application/json' \
  -d '{
    "agent": "claude",
    "prompt": "Fix the failing test.",
    "work_dir": "/path/to/project",
    "volumes": ["/path/to/hooks:/hooks:ro"],
    "lifecycle": {
      "pre_init": "/hooks/setup.sh",
      "post_init": "/hooks/warmup.sh",
      "sidecar": "/hooks/watchdog.sh",
      "post_run": "/hooks/cleanup.sh"
    }
  }'
```

## Session Request Fields

### `volumes`

Optional string array of Docker bind mounts in `host:container[:mode]` format. These are merged with the existing `mounts` field at request time.

```json
"volumes": [
  "/host/hooks:/hooks:ro",
  "/host/data:/workspace/data:rw"
]
```

This is equivalent to using the structured `mounts` field:

```json
"mounts": [
  {"host": "/host/hooks", "container": "/hooks", "mode": "ro"},
  {"host": "/host/data", "container": "/workspace/data", "mode": "rw"}
]
```

Use whichever format suits your integration. Both can be used simultaneously.

### `lifecycle`

Optional object specifying hook scripts. All fields are optional — missing hooks are silently skipped. Scripts must already exist at the specified paths (typically mounted via `volumes`).

| Field | Type | Description |
|-------|------|-------------|
| `pre_init` | `string` | Runs BEFORE the agent binary starts. Blocking. Non-zero exit = session fails. |
| `post_init` | `string` | Runs AFTER the agent is alive but BEFORE the first prompt. Blocking. Non-zero exit = session fails. |
| `sidecar` | `string` | Spawned as a background process alongside the agent. Killed when agent exits. |
| `post_run` | `string` | Runs AFTER the agent exits, before container teardown. Non-zero exit is logged only. |
| `hook_timeout` | `int` | Timeout in seconds for blocking hooks. Default: 30. `post_run` uses 2x this value. |

## Lifecycle Sequence

```text
1. Container created (volumes mounted)
2. pre_init script runs (blocking, timeout)
   → exit 0: continue
   → exit non-0: session fails, agent never starts
3. Agent binary starts (claude/codex)
4. post_init script runs (blocking, timeout)
   → exit 0: continue to prompt delivery
   → exit non-0: agent killed, session fails
5. First prompt delivered to agent
6. sidecar script spawned in background
7. Agent runs normally (streaming events, tool use, etc.)
8. Agent exits
9. sidecar receives SIGTERM, 5s grace, then SIGKILL
10. post_run script runs (blocking, 2x timeout)
    → stdout captured in session log
    → exit code logged but does not affect session status
11. Container teardown
```

## Hook Environment Variables

All hooks receive:

| Variable | Value |
|----------|-------|
| `SESSION_ID` | Agentruntime session ID |
| `TASK_ID` | Task ID from session request |
| `AGENT` | Agent name (`claude`, `codex`) |
| `WORK_DIR` | Working directory path |
| `AGENT_PID` | Agent process PID (post_init and sidecar only) |

## Hook Output in Session Logs

Hook stdout and stderr appear in the session's NDJSON event stream as `system` events with a `source` tag:

```json
{"type": "system", "data": {"source": "hook:pre_init", "text": "Cloning repo..."}}
{"type": "system", "data": {"source": "hook:sidecar", "text": "Cost: $0.42"}}
{"type": "system", "data": {"source": "hook:post_run", "text": "Extracted 12 files"}}
```

These events flow through the normal NDJSON log and WebSocket bridge, so any client consuming session output will see hook activity inline with agent events.

## Use Cases

### Workspace Setup (pre_init)

Clone a repository and install dependencies before the agent starts:

```bash
#!/bin/sh
git clone --depth 1 "$REPO_URL" "$WORK_DIR/project"
cd "$WORK_DIR/project" && npm install --silent
```

### Cost Watchdog (sidecar)

Monitor task cost and kill the agent if it exceeds a budget:

```bash
#!/bin/sh
BUDGET_USD=${COST_BUDGET:-5.0}
while kill -0 "$AGENT_PID" 2>/dev/null; do
  COST=$(curl -s "http://host.docker.internal:8801/api/v1/tasks/$TASK_ID" \
    | jq -r '.cost_usd // 0')
  if [ "$(echo "$COST > $BUDGET_USD" | bc -l)" -eq 1 ]; then
    echo "Cost $COST exceeds budget $BUDGET_USD — killing agent"
    kill "$AGENT_PID"
    exit 0
  fi
  sleep 10
done
```

### Artifact Extraction (post_run)

Copy outputs to a shared volume after the agent finishes:

```bash
#!/bin/sh
cp -r "$WORK_DIR/output/" /workspace/data/artifacts/
echo "Extracted $(find /workspace/data/artifacts -type f | wc -l) files"
```

### Security Sandbox (pre_init)

Write a restrictive `settings.json` for non-owner sessions:

```bash
#!/bin/sh
cat > /home/user/.claude/settings.json << 'EOF'
{
  "permissions": { "deny": ["Bash", "Write", "Edit"] },
  "hooks": {}
}
EOF
```

## Runtime Support

| Runtime | Volumes | Lifecycle hooks |
|---------|---------|----------------|
| `docker` | Bind-mounted into container | Executed inside container by sidecar |
| `local` (sidecar) | N/A (host filesystem) | Executed on host by sidecar |
| `local-pipe` (legacy) | N/A | Not supported |

## Error Handling

- **Blocking hooks** (pre_init, post_init): Non-zero exit code fails the session. The session state becomes `failed` with `exit_code: 1`.
- **Background hook** (sidecar): If the sidecar hook crashes, it is logged but does not affect the agent session.
- **post_run**: Non-zero exit is logged but does not change the session's final status.
- **Missing scripts**: If the specified path does not exist, the hook is silently skipped.
- **Timeout**: Blocking hooks are killed after the configured timeout (default 30s). This is treated as a failure.

## Backward Compatibility

The `volumes` and `lifecycle` fields are fully optional. Existing session requests without them work exactly as before — no hooks are executed, no behavior changes.

## Distinction from Claude Code Hooks

**Lifecycle hooks** (this feature) run at container/session lifecycle points (before agent start, after agent exit). They are agentruntime-managed.

**Claude Code hooks** (see [hooks.md](hooks.md)) run at tool-use lifecycle points inside the Claude Code agent (PreToolUse, PostToolUse, Stop). They are Claude Code-managed via `settings.json`.

Both can be used together. Lifecycle hooks set up the environment; Claude Code hooks observe tool use within that environment.
