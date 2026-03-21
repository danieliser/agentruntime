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
| Remote Docker (`--docker-host`) | Done | `DOCKER_HOST=ssh://` or `tcp://` on all docker CLI commands |
| SSH bare metal runtime | Planned | Upload sidecar, port forward, WS dial — next major runtime |
| SSH + Docker runtime | Planned | SSH into remote host, run Docker there — flag on SSH runtime |
| WSL runtime | Deferred | Windows Subsystem for Linux — niche but real |
| Fly.io / cloud VM runtime | Deferred | Spin up Fly machines or EC2 instances per agent |
| Anthropic sandbox-runtime (SRT) | Deferred | Purpose-built agent isolation from Anthropic |
| macOS native VM (Virtualization.framework) | Deferred | Lightweight VMs on Apple Silicon |
| Kubernetes (k8s) | Deferred | Pod-per-agent — premature until scale demands it |

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

1. SSH runtime: bare metal sidecar on remote hosts via SSH (upload, port forward, WS dial)
2. SSH + Docker: run Docker commands over SSH connection (flag on SSH runtime)
3. CLI version pinning: the bundled Docker image installs the latest `claude` and `codex` CLIs at build time
4. Local mode materialization: settings.json, credentials, MCP config not written for local runtime

### Planned Runtimes

- WSL (Windows Subsystem for Linux)
- Fly.io / cloud VMs (per-agent machines)
- Anthropic sandbox-runtime (SRT)
- macOS Virtualization.framework (lightweight Apple Silicon VMs)
- Kubernetes (pod-per-agent, when scale demands)

### Deferred

- PTY-first terminal sessions as a first-class API mode
- Generated OpenAPI spec
- Multi-tenant auth and access controls

## Security & Governance (inspired by NVIDIA OpenShell)

Reference: [OpenShell GitHub](https://github.com/NVIDIA/OpenShell) · [Architecture](https://docs.nvidia.com/openshell/latest/about/architecture.html) · [Policy Schema](https://docs.nvidia.com/openshell/latest/reference/policy-schema.html)

### Network Policy Engine

| Feature | Status | Notes |
| --- | --- | --- |
| L7 HTTP method+path enforcement | Planned | Current Squid proxy operates at L4 (host:port allow/deny). Upgrade to TLS-terminating proxy that inspects HTTP method and URL path per policy rule. Enables "read this repo but don't push to it" granularity |
| Binary-level policy binding | Planned | Bind network rules to the calling binary, not just destination. Prevents a compromised agent from using `curl` to bypass restrictions meant for `claude`. OpenShell matches `(binary_path, host, port)` tuples |
| Declarative YAML network policies | Planned | Replace ad-hoc Squid ACLs with declarative YAML policy files. Named policy entries with endpoints, binaries, rules, and enforcement mode. Schema modeled on OpenShell's `network_policies` block |
| Hot-reloadable network policies | Planned | Network policies update on running containers without restart. Static policies (filesystem, process) locked at creation. Clean split avoids "do I restart the container" ambiguity |
| Audit mode (`enforcement: audit`) | Planned | Log policy violations without blocking. Dry-run mode for developing and tuning egress rules before enforcement. Essential for policy iteration workflow |
| Wildcard host matching | Planned | Support `*.example.com` in endpoint host fields for CDN and multi-subdomain services |
| Per-endpoint TLS modes | Planned | `terminate` (decrypt for L7 inspection) vs `passthrough` (raw TCP, no inspection). Not all endpoints need L7 — passthrough avoids overhead for trusted destinations |

### Credential & Inference Isolation

| Feature | Status | Notes |
| --- | --- | --- |
| `inference.local` proxy pattern | Planned | Well-known hostname that the proxy intercepts for model API calls. Strips agent-provided keys, injects real credentials from operator-configured providers. Agent never sees real API keys |
| Credential provider system | Planned | Named credential bundles injected as env vars at sandbox creation. Provider types for GitHub, Anthropic, OpenAI, etc. Credentials never touch the container filesystem. Extends current `CredentialSync` |
| Privacy router / model routing | Planned | Operator controls which model backend serves inference regardless of what the agent requests. Route sensitive prompts to local models, non-sensitive to frontier APIs. Policy-driven, not agent-driven |
| Local model backend support | Planned | Point `inference.local` at any OpenAI-compatible server (Ollama, vLLM, etc.) via config. `host.openshell.internal` pattern for host-accessible endpoints |

### Kernel-Level Isolation

| Feature | Status | Notes |
| --- | --- | --- |
| Landlock LSM filesystem enforcement | Planned | Kernel-level filesystem access control beyond Docker's default. Restrict reads/writes to approved paths. Absolute path allowlists with `read_only` and `read_write` semantics. Max 256 paths, no `..` traversal |
| Seccomp BPF process constraints | Planned | Block dangerous syscalls at kernel level. Prevent privilege escalation. Complement Docker's default seccomp profile with agent-specific restrictions |
| Non-root sandbox user | Done | Already have `--cap-drop ALL` + `no-new-privileges`. OpenShell enforces `run_as_user: sandbox` (rejects `root`/`0`). Our Docker hardening covers this |

### Agent-Initiated Policy Proposals

| Feature | Status | Notes |
| --- | --- | --- |
| Agent permission escalation requests | Planned | Agent hits a policy wall → emits a structured escalation event → routed to human approval channel (Slack thread, web UI, CLI prompt). Approved changes hot-reload into the running sandbox. **OpenShell markets this but hasn't built it** — their flow is purely operator-driven via CLI. This is the PERSIST Slack Channel → Thread → User pattern |
| Scoped approval grants | Planned | Approvals are time-limited and scope-limited (e.g., "push to this repo for the next 2 hours"). Prevents permanent escalation from one-time approvals |
| Approval audit trail | Planned | Every escalation request, approval, denial, and resulting policy change is logged with timestamps, actor, and justification. Feeds into session audit logs |

### Orchestration Architecture

| Feature | Status | Notes |
| --- | --- | --- |
| K3s gateway cluster | Deferred | OpenShell runs a K3s cluster inside Docker as its control plane, giving pod-level isolation and Kubernetes scheduling for free. Heavyweight for single-host, but potentially superior to individual `docker run` at scale. Worth revisiting when managing 10+ concurrent agent containers or expanding to multi-node |
| Pod-per-agent scheduling | Deferred | Kubernetes-native agent isolation. Each agent gets its own pod with resource limits, network policies, and sidecar injection via admission controllers. Natural evolution if K3s gateway is adopted |
| Multi-platform runtime expansion | Deferred | K3s gateway could manage agents across cloud providers, bare metal, and edge nodes from one control plane. Pairs with planned SSH and Fly.io runtimes as alternative backends behind a unified scheduler |
