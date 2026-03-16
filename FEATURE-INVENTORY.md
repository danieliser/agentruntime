# Feature Inventory

## Status Key
- **Done** — implemented, tested, live tested
- **Done (unit)** — implemented, unit tested, not live tested
- **Partial** — partially implemented, gaps noted
- **Stub** — interface defined, no implementation
- **Deferred** — not planned for near term

## Core Architecture

| Feature | Status | Notes |
|---------|--------|-------|
| Runtime interface (Spawn/Recover/Name) | Done | |
| Agent interface (BuildCmd/Name/ParseOutput) | Done | |
| ProcessHandle (Stdin/Stdout/Stderr/Wait/Kill/PID) | Done | |
| Session Manager (CRUD, lifecycle, thread-safe) | Done | |
| ReplayBuffer (ring buffer, WaitFor subscription) | Done | |
| WS Bridge (replay-based streaming) | Done | |
| HTTP API (Gin, session CRUD, WS upgrade) | Done | |
| SessionRequest types (grouped, typed agent configs) | Done | |
| Config materializer (file placement, mounts) | Done | |
| Go client SDK (pkg/client/) | Done (unit) | |
| CLI dispatch (agentd dispatch --config) | Done (unit) | |
| XDG data dir (~/.local/share/agentruntime/) | Done | |
| NDJSON log persistence (tee to replay + file) | Done | Live tested |
| Structured logging (session lifecycle events) | Done | |

## Agents

| Feature | Status | Notes |
|---------|--------|-------|
| Claude Code — BuildCmd | Done | -p, --output-format stream-json, --verbose, --resume |
| Claude Code — ParseOutput (NDJSON) | Done | Extracts result line, cost, subtype |
| Claude Code — Live tested (OAuth) | Done | |
| Claude Code — Live tested (API key) | Done | Hit usage limit, auth worked |
| Codex — BuildCmd | Done | exec --json --full-auto --skip-git-repo-check |
| Codex — ParseOutput (JSONL events) | Done | message.completed, response.completed |
| Codex — Live tested (OAuth) | Done | |
| Codex — Live tested (API key) | Done | |
| OpenCode — BuildCmd | Stub | TODO: verify CLI flags |
| OpenCode — ParseOutput | Stub | |

## Runtimes

| Feature | Status | Notes |
|---------|--------|-------|
| Local — Spawn (os/exec) | Done | Live tested |
| Local — Process group kill | Done | Unix-only, tested |
| Local — Env inheritance | Done | Parent env + extra vars |
| Docker — Spawn (docker run CLI) | Done | Labels, security flags, mounts |
| Docker — Env-file (clean-room) | Done | Only explicit vars, 0600 perms |
| Docker — Security hardening | Done | cap-drop ALL, no-new-privileges, --init |
| Docker — Resource limits | Done (unit) | --memory, --cpus |
| Docker — Container recovery (labels) | Done (unit) | docker ps --filter label= |
| Docker — Live test with real agent | **Not done** | Needs agent Docker image |
| SSH — Remote Docker over SSH | **Not done** | ~400 LOC, nice to have |
| SSH — Remote process over SSH | **Not done** | ~300 LOC, nice to have |
| OpenSandbox — WebSocket runtime | Deferred | |

## Session Preservation

| Feature | Status | Notes |
|---------|--------|-------|
| Isolated agent home per session | Done | claude-sessions/{id}/, codex-sessions/{id}/ |
| Claude session dir structure | Done | projects/{mangled-path}/, sessions/ |
| Credentials copy into session dir | Done | .credentials.json + credentials.json |
| Resume args (--resume --session-id) | Done | Reads from sessions/*.json or .jsonl mtime |
| Codex session dir | Done | |
| Session pruning (retention window) | Done | |
| GET /sessions/:id/info (host paths) | Done | session_dir, log_file, ws_url, log_url |
| GET /sessions/:id/log (NDJSON file) | Done | application/x-ndjson |

## Credentials

| Feature | Status | Notes |
|---------|--------|-------|
| Claude OAuth — Keychain extraction (macOS) | Done | security find-generic-password |
| Claude OAuth — Cache with 30s throttle | Done | |
| Claude OAuth — Watch mode (background refresh) | Done | |
| Codex OAuth — ~/.codex/auth.json detection | Done | |
| API key passthrough (env) | Done | ANTHROPIC_API_KEY, OPENAI_API_KEY |
| Linux fallback (manual file placement) | Done | |
| --credential-sync daemon flag | Done | |

## API Endpoints

| Endpoint | Status | Notes |
|----------|--------|-------|
| GET /health | Done | |
| POST /sessions | Done | Full SessionRequest shape |
| GET /sessions | Done | List all sessions |
| GET /sessions/:id | Done | Session status |
| GET /sessions/:id/info | Done | Host paths, URLs |
| GET /sessions/:id/logs?cursor=N | Done | Polling with cursor advancement |
| GET /sessions/:id/log | Done | Full NDJSON file download |
| DELETE /sessions/:id | Done | Kill + state update |
| GET /ws/sessions/:id | Done | WebSocket streaming |

## Testing

| Category | Count | Notes |
|----------|-------|-------|
| Unit tests | ~200 | All packages |
| Adversarial tests | ~50 | Edge cases, malformed input |
| Security tests | ~30 | Env isolation, injection, resource exhaustion |
| Fuzz targets | 6 | ParseOutput, ReplayBuffer, materializer, client, API |
| Integration tests | ~20 | Full WS lifecycle, API CRUD |
| Live tests | 4 | Claude OAuth, Claude API key, Codex OAuth, Codex API key |
| Total | 299 | All passing with -race |

## Gaps — Priority Order

### P0: Required for production use

1. **Interactive/steering mode** — stdin kept open, WS stdin frames routed to process.
   Currently stdin is closed at spawn. The bridge infrastructure supports it (stdinPump
   exists) — need to remove the close, add a session mode flag, update tests.
   ~100 LOC + test updates.

2. **Orphan recovery stdio reattach** — after daemon restart, recovered Docker containers
   are in the session registry but their stdout isn't being streamed. Need to reattach
   via `docker logs --follow` and pipe to replay buffer.
   ~150 LOC.

3. **Agent Docker image** — port Dockerfile.agent from PAOP (node:22-slim, claude + codex
   installed, tini, UID matching). Also port Dockerfile.proxy (squid + domain allowlist).
   Copy + adapt.

4. **E2E test suite** — automated tests that start the daemon, create sessions with real
   agents (or lightweight mocks), verify WS streaming, log persistence, kill, resume.
   Currently live tests are manual Python scripts.

### P1: Nice to have

5. **SSH runtime — remote Docker** — SSHRuntime.Spawn() dials SSH, runs docker run on
   remote host. For Proxmox/VM isolation. golang.org/x/crypto/ssh. ~400 LOC.

6. **SSH runtime — remote process** — same SSH connection, runs agent directly without
   Docker on remote host. Lighter. ~300 LOC.

### Deferred

- OpenSandbox runtime (WebSocket to execd)
- OpenCode agent implementation
- PTY support (interactive terminal sessions)
- Web UI / dashboard
- OpenAPI spec generation
- Multi-tenant access controls
- Rate limiting on API
- Container snapshot/restore
