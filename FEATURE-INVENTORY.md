# Feature Inventory

## Status Key

- **Done**: implemented in the main code path
- **Done (unit)**: implemented and covered in unit-style tests, but not called out as separately live-verified here
- **Partial**: usable, but with clear agent/runtime-specific gaps
- **Stub**: schema or interface exists, but the behavior is not implemented yet
- **Deferred**: intentionally not in the active delivery path right now

## Core Architecture

| Feature | Status | Notes |
| --- | --- | --- |
| Runtime interface (`Spawn` / `Recover` / `Name`) | Done | Shared contract for local, Docker, and future runtimes |
| Agent interface (`BuildCmd` / `Name` / `ParseOutput`) | Done | Claude, Codex, OpenCode stub |
| ProcessHandle abstraction | Done | StdIO, lifecycle, recovery metadata |
| Session manager + replay buffer | Done | Session lifecycle, replay, cursor-based reconnect |
| Persistent NDJSON session logs | Done | Replay buffer is mirrored to on-disk logs |
| HTTP API | Done | Session CRUD, logs, info, health |
| Daemon WebSocket bridge | Done | `/ws/sessions/:id` for replay plus stdin |
| Sidecar v2 WebSocket server (`cmd/sidecar/`) | Done | `/ws` plus `/health` |
| Normalized event schema | Done | `agent_message`, `tool_use`, `tool_result`, `result`, `progress`, `system`, `error`, `exit` |
| Structured output normalization | Done | Claude and Codex mapped into one schema |
| Streaming deltas | Done | Claude partial text events are emitted as `agent_message` with `delta: true` |
| Steering and interrupt control | Done | Sidecar routes `prompt`, `interrupt`, and `steer` |
| Context injection | Done | Claude via embedded MCP IDE bridge; Codex currently no-ops with a warning |
| Mention injection | Done | Same behavior as context injection |
| Unified `SessionRequest` schema | Done | Shared by HTTP, Go client, and `agentd dispatch` |
| Config materializer | Done | Writes per-session Claude/Codex homes and MCP config |
| Go client SDK (`pkg/client`) | Done (unit) | HTTP client plus log streaming helper |
| CLI dispatch (`agentd dispatch --config`) | Done (unit) | YAML in, HTTP session dispatch out |
| XDG data dir (`~/.local/share/agentruntime`) | Done | Override with `AGENTRUNTIME_DATA_DIR` |
| Local sidecar runtime | Done | Default `local` runtime path |
| Legacy local pipe runtime | Done | `local-pipe` retained for backwards compatibility |

## Agents

| Feature | Status | Notes |
| --- | --- | --- |
| Claude Code prompt mode | Done | `claude -p ... --output-format stream-json --verbose` |
| Claude Code interactive sidecar mode | Done | `--input-format stream-json` plus embedded MCP server |
| Claude structured output | Done | Assistant, result, progress, and streaming delta mapping |
| Claude tool events | Partial | `tool_use` is normalized; no separate `tool_result` event today |
| Claude resume session lookup | Done | Reads Claude session metadata on disk |
| Codex prompt mode | Done | `codex exec --json --full-auto --skip-git-repo-check` |
| Codex interactive sidecar mode | Done | `codex app-server --listen stdio://` |
| Codex structured output | Done | Notifications normalized into shared event types |
| Codex tool events | Done | `tool_use` and `tool_result` normalized |
| Codex resume session lookup | Done | Reads local Codex session history |
| OpenCode agent implementation | Stub | Interface exists, CLI flags and parsing still not verified |

## Runtimes

| Feature | Status | Notes |
| --- | --- | --- |
| Local host execution | Done | Default runtime is sidecar-backed local execution |
| Local runtime recovery | Partial | Local sidecar processes do not survive daemon restart |
| Docker sidecar execution | Done | Containerized sidecar with health check and WS dial |
| Docker config materialization | Done | Claude/Codex homes mounted per session |
| Docker clean-room env-file | Done | Only explicit env vars plus managed proxy vars are injected |
| Docker security hardening | Done | `--init`, `--cap-drop ALL`, `--cap-add DAC_OVERRIDE`, `no-new-privileges` |
| Docker resource limits | Done (unit) | `memory`, `cpus` |
| Docker orphan recovery | Done | Surviving containers are rediscovered on daemon restart |
| Docker stdio reattach after recovery | Done | Recovered sessions reattach logs/stdout into replay |
| Docker managed network | Done | `agentruntime-agents` |
| Docker managed proxy | Done | `agentruntime-proxy` Squid sidecar |
| Docker live-tested flow | Done | Claude and Codex authenticated and exercised in containerized sessions |
| SSH runtime | Stub | Not implemented yet |
| OpenSandbox runtime | Deferred | Interface exists, active implementation is deferred |

## Session Preservation

| Feature | Status | Notes |
| --- | --- | --- |
| Isolated Claude session homes | Done | Per-session directories under the daemon data dir |
| Isolated Codex session homes | Done | Per-session directories under the daemon data dir |
| Credentials copy/materialization | Done | Claude credentials and Codex auth are materialized when available |
| Resume session handoff | Done | `resume_session` maps to Claude/Codex native session IDs |
| Session pruning helpers | Done | Agent-specific session housekeeping exists |
| Session info endpoint | Done | `/sessions/:id/info` exposes host paths and convenience URLs |
| Incremental replay polling | Done | `/sessions/:id/logs?cursor=N` |
| Full log download | Done | `/sessions/:id/log` returns the persisted NDJSON log |

## Credentials And Config

| Feature | Status | Notes |
| --- | --- | --- |
| Claude OAuth sync from host | Done | Keychain on macOS plus file-based fallbacks |
| Codex auth discovery | Done | Reads `~/.codex/auth.json` when present |
| Background credential sync | Done | `--credential-sync` daemon flag |
| Claude `CLAUDE.md` materialization | Done | Written into the session home |
| Claude `settings.json` materialization | Done | Includes skip-dangerous-mode prompt default |
| Claude MCP merge + sanitization | Done | Merges explicit `mcp_json` plus `mcp_servers` |
| Codex `config.toml` materialization | Done | Plus trusted `/workspace` defaults |
| Codex `instructions.md` materialization | Done | Written into session home |

## API Surface

| Endpoint | Status | Notes |
| --- | --- | --- |
| `GET /health` | Done | Status plus selected runtime |
| `POST /sessions` | Done | Creates session from `SessionRequest` |
| `GET /sessions` | Done | Lists sessions |
| `GET /sessions/:id` | Done | Raw session snapshot |
| `GET /sessions/:id/info` | Done | Session details plus host paths |
| `GET /sessions/:id/logs` | Done | Incremental log/replay fetch |
| `GET /sessions/:id/log` | Done | Full log download |
| `DELETE /sessions/:id` | Done | Kill session |
| `GET /ws/sessions/:id` | Done | Daemon bridge |

## Testing

These counts are source-counted from the repository on 2026-03-17 (post-hardening).

| Category | Count | Notes |
| --- | --- | --- |
| Test functions (`func Test...`) | 381 | Across the repo |
| Fuzz targets (`func Fuzz...`) | 17 | API, client, bridge, materializer, session, agent parsing, sidecar normalization |
| Adversarial test functions | 85 | Files named `*adversarial*_test.go` |
| E2E test functions | 17 | `pkg/e2e` |
| Go packages | 12 | `go list ./...` |

## Remaining Gaps

### Highest Priority

1. SSH runtime: still not implemented for either remote process execution or remote Docker execution.
2. CLI version pinning: the bundled Docker image installs the latest `claude` and `codex` CLIs at build time; there is no explicit version pinning yet.
3. OpenCode: still a stub at the agent layer and not part of the v2 sidecar path.
4. Comparative review against other toolchains: agentruntime has not yet been systematically reviewed against adjacent tools and runtimes to close remaining ergonomic or architecture gaps.

### Deferred Or Secondary

- OpenSandbox runtime implementation
- PTY-first terminal sessions as a first-class API mode
- Generated OpenAPI spec
- Web UI / dashboard
- Multi-tenant auth and access controls
