# Changelog

All notable changes to agentruntime are documented in this file.

## [0.8.0] — 2026-03-25

### Features
- **Container lifecycle hooks**: New `lifecycle` field on `SessionRequest` with four hook points —
  `pre_init` (before agent start), `post_init` (after agent init), `sidecar` (background process),
  and `post_run` (after agent exit). Hooks execute inside the sidecar process, emit output as
  `system` events in the NDJSON stream, and support configurable timeouts. Pre-init and post-init
  failures are fatal (session fails with exit code 1). Works on both local and Docker runtimes.
- **Volumes convenience field**: New `volumes` string array on `SessionRequest` accepts Docker's
  `host:container[:mode]` syntax. Parsed into `Mount` structs and merged with existing `mounts`
  field. Simplifies bind-mount configuration for orchestrators.
- **Hook environment variables**: All hooks receive `SESSION_ID`, `TASK_ID`, `AGENT`, and
  `WORK_DIR`. Post-init and sidecar hooks also receive `AGENT_PID`.
- **Sidecar hook process management**: Background sidecar hooks receive SIGTERM when the agent
  exits, with a 5-second grace period before SIGKILL.
- **Session identity env vars**: `SESSION_ID` and `TASK_ID` are now passed to both Docker and
  local sidecar runtimes for use by hooks and agent configuration.

### Documentation
- New guide: [Container Lifecycle Hooks](docs/guides/lifecycle-hooks.md) — full reference with
  lifecycle sequence, environment variables, use cases (workspace setup, cost watchdog, artifact
  extraction, security sandbox), and error handling.

## [0.7.0] — 2026-03-22

### Features
- **Team spawning support**: New `TeamConfig` on `SessionRequest` enables Claude Code's Agent
  Teams inbox protocol. When set, agentruntime passes `--agent-id`, `--agent-name`, `--team-name`
  flags and sets `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1` on the Claude process. Orchestrators
  scaffold team directories; agentruntime validates and spawns.
- **Docker team directory mount**: Team sessions on Docker automatically bind-mount
  `~/.claude/teams/{name}/` into the container so the Claude binary can read/write inbox files.
- **Session team metadata**: `SessionInfo` and `SessionSummary` expose team name and agent name.
  Sessions are auto-tagged with `team:{name}` and `team_agent:{agent_name}` for filtering.
- **Bare mode**: New `bare` field on `ClaudeConfig` passes `--bare` flag to Claude Code
  (skip hooks, plugins, LSP, automem, CLAUDE.md — clean room mode).
- **Validation**: Team sessions require `agent_name`, validate team directory exists on disk,
  and auto-generate `agent_id` in `name@team` format if not provided.

## [0.6.5] — 2026-03-20

### Bug Fixes
- **Chat resume not wired** (#8): `lookupResumeSessionID` dropped pre-resolved Claude session
  IDs from the chat manager because no agentruntime session matched and the Docker volume
  filesystem scan returned empty. Now passes the raw ID through as a fallback — the chat
  manager already resolved it to the correct Claude session ID.

## [0.6.4] — 2026-03-20

### Bug Fixes
- **Docker batch chats stuck "running" permanently** (#7): `watchSessionLoop` only transitioned
  chats to idle on process exit, but chat sessions spawn interactive (process never exits between
  turns). Now transitions to idle on result events (turn completion). The old interactive session
  is killed after transitioning. The ticker remains as a safety net for crashes.

## [0.6.3] — 2026-03-20

### Bug Fixes
- **Chat stuck "running" after session exit** (#7): Three recovery gaps in the chat manager —
  `respawnAfterMissing` now starts a session watcher on the new session (was missing, leaving
  chats permanently stuck); `watchSessionLoop` detects nil handles on running sessions and
  treats them as exits; `injectStdin` failures now trigger a respawn instead of a 500 error.
- **PendingMessage 30–45s clearing delay** (#6): Root cause was the same missing watcher —
  result signals from the respawned session were never consumed, so PendingMessage only cleared
  when the session fully exited. Now clears within one poll cycle (~2s).

## [0.6.2] — 2026-03-20

### Bug Fixes
- **Result channel race condition** (#6): Replaced single-fire `NotifyResult` channel with
  close-and-replace pattern. Each `NotifyResult()` call closes the current channel (waking all
  watchers) and creates a fresh one for the next turn. `watchSessionLoop` re-fetches
  `ResultCh()` each iteration to track the latest channel.

## [0.6.1] — 2026-03-20

### Bug Fixes
- **PendingMessage not clearing on result event**: Added `NotifyResult` / `ResultCh` mechanism
  on sessions. `parseAndTrackEvent` fires `NotifyResult()` on result events. `watchSessionLoop`
  selects on `ResultCh()` to clear PendingMessage mid-session without waiting for full exit.
- **Docker resume_session**: Session tags now carry `claude_session_id` for cross-respawn resume.
  `handleSessionExit` captures it from the exiting session's tags.

## [0.6.0] — 2026-03-20

### Terminal UI (agentd-tui)
- **Bubble Tea TUI client**: `agentd-tui` — full terminal client with glamour markdown rendering, debounced re-renders, and scroll-follow mode.
- **Session history**: Loads prior chat history from the logs API on connect.
- **Auto-reconnect**: Reconnects to dead sessions automatically on next message.
- **--create flag**: Creates a new chat session if none exists.
- **Session context**: TUI sessions resume with prior Claude session ID for continuity.
- **Event filtering**: Suppresses result/exit noise; shows `You:` prefix on user turns.
- Marked experimental; known gaps documented.

### Named Persistent Chats
- **Chat API**: Full REST API for named chat sessions — create, attach, delete, config PATCH.
- **CLI commands**: `agentd chat` subcommands including `attach` for interactive TUI.
- **Daemon wiring**: Chat registry and manager integrated into agentd lifecycle (Phases 1–7).
- **Session respawn**: Automatic respawn on disconnect; WatchSession starts on initial spawn.
- **Config deep-merge**: PATCH config deep-merges rather than replacing.
- **Claude resume**: Session ID wired across chat respawns for conversation continuity.

### Dashboard Embedded
- **go:embed**: Dashboard assets embedded directly into the agentd binary — no external file dependency.
- **Session history tab**: Log-based backend serving session history in the dashboard.
- **Active tab fix**: Filters out completed/failed sessions from the Active view.

### Cost Estimation
- **Token-based cost**: Cost estimation from token counts on result events.
- **cost_usd capture**: `cost_usd` captured from Claude's native event output when present.

### Config Auto-Discovery
- **Claude Code & Codex cascade**: Full replication of Claude Code and Codex CLI config discovery rules — project root walk, home dir, XDG, env overrides.

### Install & Service Management
- **Install scripts**: `install.sh` / `uninstall.sh` with launchd (macOS) and systemd (Linux) service wiring.
- **PATH setup**: Expands `~` in launchd plist, adds `~/.local/bin` to PATH.
- **Reinstall**: `rm` before `cp` to avoid macOS cached-binary kills; includes agentd-tui.

### Interactive Session Fixes
- **Stdin routing**: Route stdin through `SendPrompt` for interactive sidecar sessions.
- **Result grace disabled**: Interactive sessions no longer hit the result grace-period kill.
- **PendingMessage guard**: Marks pending on stdin injection to prevent concurrent floods.
- **NDJSON in stdout**: Handle raw NDJSON in stdout frames, not just base64.

### Bug Fixes
- **AGENT_PROMPT newlines** (Docker): Base64-encode prompt in env var; sidecar decodes. Fixes Docker rejecting prompts containing newlines.
- **Replay coalescing**: Coalesce replay deltas instead of skipping; filter heartbeat/hook noise.
- **Delta skip during replay**: Skip delta chunks during replay and suppress duplicate result events.
- **Arrow keys / mouse scroll**: TUI viewport now accepts arrow keys and mouse scroll.
- **Codex bubblewrap**: Disable Codex bubblewrap sandbox inside Docker containers.
- **Data race in attach tests**: Pass stdin explicitly instead of swapping global.
- **PyPI workflow**: `skip-existing` to handle retagged releases.

## [0.5.0] — 2026-03-19

### Stall Detection
- **Two-phase stall detection**: Advisory warning at 10 min of silence, hard kill at 50 min. Result marker early exit: force-kill after 10s grace when agent emits result but process doesn't exit (fixes hung MCP servers).
- **Configurable**: `stall_warning_timeout`, `stall_kill_timeout`, `result_grace_period` via `AGENT_CONFIG`. Set `-1` to disable individual phases.

### Session Persistence
- **Named Docker volumes**: `agentruntime-vol-{sessionID}` mounted at `~/.claude/projects/` for session continuity across container restarts. Enables `--resume` across container lifecycles.
- **API**: `persist_session: true` on SessionRequest. Volume reuse for resumed sessions. `DELETE /sessions/:id?remove_volume=true` for explicit cleanup.

### WebSocket Log Forwarding
- **NDJSON log catch-up**: When replay buffer wraps on long sessions, bridge falls back to disk-based NDJSON log file. `gap` flag on replay frames signals missing data.
- **Daemon restart recovery**: Recovered sessions restore from NDJSON log files.

### Dashboard
- **Ops dashboard** at `/dashboard/`: dark theme, auto-refreshing sessions table, click-to-detail with live WS event log, session metrics (tokens, tools, cost, uptime).
- **Branding**: SVG wordmark, GitHub/issues links, footer with report-issue link.
- **Vanilla JS**: No build tools, three files (HTML, JS, CSS).

### Rich System Events
- **Heartbeat**: Every 10s with uptime, tokens, tool calls, agent status. Flows through replay buffer.
- **Tool call tracking**: New counter in event metrics.

### Error Handling
- **Codex JSONL false-positive prevention**: `ClassifyFromEvents()` filters to `agent_message` and `error` types only, ignoring embedded MCP results.
- **Fatal error fast-fail**: Sidecar detects repeated auth errors from stderr, emits `fatal: true` error event, kills agent immediately. Consumers stop waiting.
- **New patterns**: `refresh_token_reused`, `Failed to refresh token`, `API usage limits`.

### Multi-Runtime
- **Both runtimes active simultaneously**: `--runtime` sets default, both `local` and `docker` always available. Callers select per-session via `runtime` field.
- **Recovery on all runtimes**: Not just primary.
- **Cleanup on all runtimes**: `errors.Join()` aggregation at shutdown.

### Codex Token Refresh
- **Proactive refresh**: JWT expiry checking with 24h threshold. Automatic refresh via OpenAI's Auth0 endpoint. 12h check interval. Atomic file writes.
- **Graceful degradation**: Logs actionable "run codex login" message on failure.

### Security
- **API key stripping**: Local runtime strips `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` from sidecar env so agents use OAuth.
- **Host state isolation**: Removed blind copy of `~/.claude.json` into session dirs.
- **Mount pre-creation**: Single-file bind-mount sources auto-created to prevent Docker directory creation.
- **Materializer guard**: Auto-infers claude/codex config block from agent name.
- **AGENT_PROMPT**: Docker runtime passes prompt via env for fire-and-forget mode so Claude exits after result.
- **Session ID passthrough**: Callers can set `session_id` on requests (valid UUID, rejects duplicates).

### Documentation
- Six configuration guides (Claude, Codex, MCP, env vars, hooks, skills/agents/plugins)
- Credential architecture guide
- Research-backed specs for all features
- Auto-discovery spec (773 lines) with exact Claude Code and Codex CLI cascade rules

## [0.4.0] — 2026-03-18

### Security
- **Error classification**: 12 regex patterns classify agent errors (model_not_found, auth_error, rate_limit, etc.) with `error_category` and `retryable` on exit events.
- **Project root validation**: Reject sensitive dirs, path traversal, filesystem root.
- **iptables isolation**: Linux inter-container lateral movement blocked.
- **Startup crash detection**: Zero tokens + minimal output heuristic.
- **Timestamp fix**: WS events now always carry timestamps.

### Session Improvements
- **Runtime metrics API**: last_activity, tokens, cost, tool calls, uptime on `/sessions/:id/info`.
- **`--effort` flag** for Claude agent.
- **NO_PROXY `host-gateway`** for Linux Docker.

## [0.3.0] — 2026-03-17

### Plumbing Gap Audit & Fixes
- **AGENT_CONFIG envelope**: Sidecar now reads `AGENT_CONFIG` env var for model, resume_session, env, max_turns, allowed_tools, approval_mode. Full config passthrough from daemon to agent CLI.
- **Bridge protocol upgrade**: Daemon bridge now supports `steer`, `interrupt`, `context`, `mention` command types in addition to `stdin`. All five sidecar commands are reachable from clients.
- **Session lifecycle**: `handleDeleteSession` now calls `Remove()` — sessions no longer accumulate in memory forever. Added `ShutdownAll()` for graceful daemon shutdown. Configurable `--max-sessions` limit.
- **Credential auto-discovery**: Materializer automatically finds Claude credentials from sync cache (`{dataDir}/credentials/`) or host (`~/.claude/.credentials.json`). Same for Codex (`~/.codex/auth.json`).
- **Dead code removed**: OpenCodeAgent, OpenSandboxRuntime, unsupportedBackend all removed. `mustRawMessage` panic replaced with error return.
- **Docker proxy lifecycle**: `NetworkManager.Cleanup()` called on graceful shutdown. Proxy container stopped when daemon exits.
- **Log extension fix**: Info endpoint now returns correct `.ndjson` extension.
- **Exit error_detail**: `ExitResult` now carries `ErrorDetail` string from sidecar exit frames.

### Dashboard
- **v3 rewrite**: Spawn-on-demand with dropdown count selectors (1/5/10/15/20/30)
- **Event stream modal**: Click any session card to see live NDJSON event stream with type filters (user_message, agent_message, tool_use, tool_result, result, progress, error, system)
- **User messages in stream**: Initial prompt and steers appear as teal `user_message` entries
- **System init expanded**: First system event shows model, tools count, MCP servers, agents, permissions mode
- **TTFT benchmark**: One-click benchmark with dropdown for Claude default/Haiku/Sonnet, Codex, All Combinations
- **Session actions**: Steer (interactive), retry (kill + respawn), resume (new session inheriting old context), kill per session
- **Kill-all split**: Dropdown with Kill All Claude / Kill All Codex
- **Config panel**: Custom prompt, env vars, CLAUDE.md, MCP servers, container settings
- **Performance timing**: boot_ms, ttft_ms, duration_ms per session with averages in stats bar
- **localStorage persistence**: Session IDs survive page refresh, auto-recovered from agentd on reload
- **Benchmark tracking**: Benchmark sessions tracked in dashboard state, properly cleaned up

### Testing
- 498 tests (up from 381), 25 fuzz targets, 100+ adversarial tests
- New test suites: bridge steerable commands, AGENT_CONFIG adversarial, session lifecycle adversarial, credential auto-discovery, concurrent use cases, fuzz targets for new surfaces
- Compile-time interface assertions for all key types

## [0.2.1] — 2026-03-16 (evening)

### Session Preservation
- Per-session Claude/Codex home directories under daemon data dir
- Claude session discovery via `sessions/*.json` mtime-based lookup
- Codex session discovery via local history
- `resume_session` field wired through to `--resume --session-id` flags
- Credential sync from macOS Keychain (`--credential-sync` daemon flag)
- Session info endpoint (`GET /sessions/:id/info`) with host paths and URLs

### Docker Hardening
- Orphan container recovery on daemon restart via Docker labels
- NDJSON log replay from recovered sessions
- Container mount path fix (`/root/` → `/home/agent/`)
- `.claude.json` trust bypass (onboarding, workspace trust, dangerous mode skip)
- Docker env-file lifecycle fix (cleanup after container reads, not before)

### Bug Fixes
- Fix replay buffer data race in bridge readPump
- Fix session ID path traversal in temp dir names (dots in sanitization)
- Fix local runtime empty env (nil vs empty slice)
- Fix Docker env-file leaking parent env
- Fix import cycle materializer→api→runtime→materializer

## [0.2.0] — 2026-03-16 (afternoon)

### Sidecar v2 Architecture
- `cmd/sidecar/` — Go binary that runs inside Docker containers
- Claude dual-channel backend: JSONL stdio (`--output-format stream-json --input-format stream-json`) + embedded MCP IDE bridge
- Codex app-server backend: JSON-RPC via `codex app-server --listen stdio://` + `codex exec --json` for prompt mode
- Normalized event schema: `agent_message`, `tool_use`, `tool_result`, `result`, `progress`, `system`, `error`, `exit`
- Streaming deltas via `--include-partial-messages`
- Context injection via `context` and `mention` sidecar commands
- Generic command backend for arbitrary agents
- `wsHandle` — WS-backed ProcessHandle shared by local and Docker runtimes

### Docker Runtime
- Detached container mode (`-d`) with port mapping (`-p 0:9090`)
- Managed `agentruntime-agents` bridge network
- Squid proxy sidecar (`agentruntime-proxy`) for egress control
- Config materialization into per-session container homes
- Security hardening: `--init`, `--cap-drop ALL`, `--cap-add DAC_OVERRIDE`, `no-new-privileges`
- Health check loop with sidecar `/health` endpoint
- Multi-stage Dockerfile with Go sidecar build

### Local Sidecar Runtime
- Local runtime uses same sidecar v2 protocol as Docker
- Host process sidecar with free port allocation and WS dial
- Same normalized events whether local or Docker

### Network Manager
- `sync.Once` for concurrent proxy setup (race fix)
- `--type container` for Docker inspect disambiguation
- "Already exists" treated as success for idempotent network creation

## [0.1.0] — 2026-03-16 (morning)

### Core Architecture
- Runtime interface (`Spawn`, `Recover`, `Name`)
- Agent interface (`BuildCmd`, `Name`, `ParseOutput`)
- ProcessHandle abstraction (StdIO, lifecycle, recovery metadata)
- Session manager with replay buffer (1 MiB, condition-variable based)
- Persistent NDJSON session logs mirrored from replay buffer

### HTTP API
- `POST /sessions` — create session from `SessionRequest`
- `GET /sessions` — list sessions
- `GET /sessions/:id` — raw session snapshot
- `DELETE /sessions/:id` — kill session
- `GET /sessions/:id/logs?cursor=N` — incremental log polling
- `GET /sessions/:id/log` — full log download
- `GET /ws/sessions/:id` — daemon WebSocket bridge

### Agents
- Claude Code: prompt mode (`-p`) and interactive mode
- Codex: prompt mode (`exec --json`) and interactive mode (`app-server`)
- Structured output parsing for both agents

### SessionRequest Schema
- Agent, prompt, work_dir, mounts, env, timeout, interactive, PTY
- `claude` config block: settings_json, claude_md, mcp_json, credentials_path
- `codex` config block: config_toml, instructions, approval_mode
- `mcp_servers` array with `${HOST_GATEWAY}` resolution
- `container` config: image, memory, cpus, network, security_opt

### Go Client SDK
- HTTP client for session CRUD + log streaming
- `agentd dispatch --config` CLI for YAML-based dispatch

### Testing Foundation
- Replay buffer, local runtime, bridge, agent parsing tests
- Adversarial tests for API, materializer, client, Docker runtime
- Fuzz targets for API, client, bridge, materializer, session, agents
- E2E subprocess daemon tests

---

## Dashboard Changelog

### [dashboard 0.3.0] — 2026-03-17

- **Spawn-on-demand**: Empty grid with spawn buttons, agents created on click
- **Dropdown counts**: 1/5/10/15/20/30 per agent/mode combination
- **Event stream modal**: Live NDJSON viewer with type filters, auto-scroll
- **User messages**: Initial prompt + steers visible in event stream (teal italic)
- **System init**: Model, tools, MCP servers, permissions expanded on first event
- **TTFT benchmark**: One-click with model dropdown, streaming results
- **Session actions**: Steer, retry, resume, kill per card
- **Config injection**: Custom prompt, env vars, CLAUDE.md, MCP servers, container settings
- **Performance timing**: boot_ms, ttft_ms, duration_ms per session
- **Kill-all split**: By agent type (Claude/Codex)
- **localStorage**: Session IDs persist across page refresh
- **Benchmark tracking**: Benchmark sessions properly tracked and cleaned up

### [dashboard 0.2.0] — 2026-03-17

- Initial concurrency dashboard — 30-session WS monitor with steering
- Session cards with blinking status dots, token counts, last text preview
- Steer button for interactive sessions
- Kill individual + kill all

### [dashboard 0.1.0] — 2026-03-17

- Auto-spawn N sessions at startup (replaced by spawn-on-demand in 0.2.0)
