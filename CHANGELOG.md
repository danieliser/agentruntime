# Changelog

All notable changes to agentruntime are documented in this file.

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
