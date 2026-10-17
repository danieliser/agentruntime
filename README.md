# AgentRuntime / AgentD

AgentD runs Claude Code and Codex as provider-native JSON sessions. It owns process execution, durable session identity, ordered event storage, replay, lifecycle controls, terminal receipts, and Docker reconstruction.

Workflow orchestration, retries, approvals, and final artifact admission belong to the caller (for example Trading Floor/DBOS).

## Current runtime model

- Claude uses bidirectional `stream-json` over stdin/stdout.
- Codex uses app-server JSON-RPC over stdio.
- Provider records are stored byte-for-byte before live publication.
- Public cursors are monotonic event sequence numbers, never byte offsets.
- Docker sessions use direct `docker attach`, retained `docker logs`, `docker wait`, and `docker inspect`—there is no execution sidecar.
- Local sessions use direct process stdio, but restart reconstruction is qualified only for Docker.

The default registry supports Claude and Codex. Old Grok/Cursor and sidecar execution methods have been retired.

## Install and run

Requirements:

- Go 1.24+
- Claude Code and/or Codex for local execution
- Docker for reconstructable isolated execution

Build and start:

```sh
go build -o agentd ./cmd/agentd
./agentd --port 8090 --runtime local
```

Or install a user service:

```sh
./install.sh
```

Useful daemon flags:

```text
--host 127.0.0.1
--port 8090
--runtime local|docker
--data-dir ~/.agentd
--plugin-config ~/.agentd/plugins.json
--docker-host ssh://user@host
--max-sessions 0
--credential-sync
--version
--build-info
--require-build VERSION@COMMIT
```

`AGENTRUNTIME_DATA_DIR` overrides the complete data root. `--data-dir` takes precedence when explicitly supplied. `AGENTRUNTIME_PLUGIN_CONFIG` overrides the external observer allowlist path when `--plugin-config` is omitted.

## Durable v1 session API

Check compatibility before submitting work:

```sh
agentd_curl() {
  curl --config <(sed 's/^/header = "Authorization: Bearer /; s/$/"/' ~/.agentd/auth.token) "$@"
}
agentd_curl -sS http://127.0.0.1:8090/api/v1/capabilities
```

AgentD binds literal `127.0.0.1` by default. Its private bearer token is created
as `~/.agentd/auth.token` with mode `0600`; private data directories use `0700`.
The helper above reads the token through a private file descriptor rather than
putting it in a URL or process argument.

The handshake reports exact build version and commit, API/event/plugin/policy
versions, listener and authentication contracts, native providers, configured
runtimes, replay persistence, Docker reconstruction, structured-output modes,
workspace profiles, maintained-container/portable-resume support, and observer
health.

## `agentctl` supervision CLI

Build the stateless CLI with `go build -o agentctl ./cmd/agentctl`. It uses the
existing v1 API and reads its bearer token from a file, never an argument or URL.
The default target is `http://127.0.0.1:38093`; `--url` or `AGENTD_URL` overrides it.
`AGENTD_TOKEN_FILE` defaults to `~/.agentd/auth.token`.

```sh
# Keep the Docker provider transport alive between turns.
./agentctl run --provider claude --model claude-haiku-4-5 --cwd "$PWD" \
  --runtime docker --keep-warm 10m --context clean \
  'Inspect the current change and summarize it'
```

`run` prints the session ID immediately. Options are `--provider claude|codex`,
`--model M`, `--effort E`, `--cwd DIR`, `--runtime local|docker` (default local),
`--keep-warm DUR` (positive Docker idle TTL, at most 24h), `--portable-resume`
(snapshot provider state before Docker cleanup), and `--context clean` (omit
ambient provider configuration). Without `--keep-warm`, a one-turn process can
exit after its reply; `prompt` wakes it through native provider resume.
Maintained sessions are available for both Claude and Codex. Portable resume is
opt-in and requires Docker; it is separate from keeping the transport warm.

For tool approval requests, add `--permissions restricted --approval-policy
on-request`. Restricted coding requires Docker and defaults to `on-request`;
`never` denies requests requiring approval. `--permissions unrestricted` requires
`never`. Omitting both preserves the daemon's provider defaults. These are
provider permission controls, not the managed research execution-policy boundary.
Both providers expose pending approvals in restricted mode.

Commands are `run`, `status`, `ls`, `since`, `grep`, `wait`, `prompt`, `steer`,
`interrupt`, `cancel`, `terminate`, `resume`, `approvals`, `approve`, `deny`,
and `result`. Use `approve ID REQUEST_ID` or `deny ID REQUEST_ID` with an ID from
`approvals ID --json`; preserve JSON quoting for string IDs. `prompt` starts a
follow-up turn; `steer` changes an active turn. Lifecycle/approval commands return
when accepted; use `wait` and output to confirm the effect. Each prompt and
lifecycle invocation is a new operation, including identical text on later turns.
`prompt` also wakes a normally completed session after its provider process
exits, using native provider resume as generation N+1. This works for local and
Docker Claude/Codex sessions without `--keep-warm`. `status --json` exposes
`continuable` and `continuable_reason`. Cancelled, terminated and unrecoverable
sessions reject new work. `resume ID PROMPT` remains explicit recovery of a lost,
nonterminal generation.

Dispatch and plan approval stay in the PERSIST CLI. Set `PERSIST_URL` (default
`http://127.0.0.1:8803`) and optionally `PERSIST_TOKEN_FILE` (default
`~/.local/share/persist-v3/initial_token.txt`) for PERSIST-aware lookup and
control. IDs can be raw session IDs, `task:T`, `plan:P`, or `plan:P/node` (node
ID or title slug). `agentctl plan show P` displays plan node state. For one
background notification per plan, approve a plan and then arm the durable plan
event waiter; after it reports an approval, act on the named node and re-arm:

```sh
persist plan auto "<plan prompt>" --cwd "$PWD" && persist plan approve <proposal_id>
agentctl wait plan:<plan_id> --until approval,failed,done --timeout 30m &
```

Use `agentctl since plan:<plan_id>` or `agentctl grep plan:<plan_id> PATTERN` to
read across its current node sessions, with each output tagged by node. Controls
for PERSIST-owned sessions route through PERSIST. Pass `--direct` to explicitly
send a control to AgentD. Place it before the session ID, for example
`agentctl --direct prompt SESSION "text"`; after a positional argument or `--`,
`--direct` is literal input. Task and plan resolution prefer `current_session_id`
and fall back to `session_id` for older PERSIST responses.

Output is compact human text by default; `--json` selects JSON. `since ID`
returns assistant text and tool summaries after its saved sequence, then advances
the cursor only after successful output. A second call with no new events prints
nothing, including in JSON mode. `since ID --raw` returns raw provider records.
`grep ID PATTERN [--type TYPE] --json` searches durable history without moving the
cursor. `result ID` returns a hash-verified structured result when available,
otherwise `{receipt, final_assistant_text}` in JSON mode. The fallback includes
only the latest turn's assistant text, which may be partial or empty after an
interrupt. A maintained session needs to end (idle expiry, cancel, or terminate)
before a terminal receipt exists. This API records administrative termination as
`state=cancelled, reason=terminated`; inspect the reason.

Cursors are independent per caller/session. Set caller identity with `--as NAME`
or `AGENTD_CALLER` (default `agentctl`). `AGENTCTL_CURSOR_DIR` overrides
`~/.agentd/agentctl/cursors`; `--cursor-dir DIR` on `since`/`wait` overrides both.
Use a separate caller for notifications so waiting does not consume unread text.
Environment options are listed in `config.example.env`; the CLI reads process
environment directly, without automatically loading that file.

For passive supervision, run this in Claude Code background Bash or Codex's
background shell, then inspect the exit code when the job wakes:

```sh
./agentctl --as supervisor-ping wait SESSION_ID \
  --until turn_end,approval,error,done --timeout 30m &
# After notification, read output using a different caller cursor:
./agentctl --as supervisor-read since SESSION_ID
```

`wait` follows the durable WebSocket stream and reconnects from its last
contiguous sequence. It prints the triggering event summary. Conditions are
comma-separated; a matching condition deliberately has a nonzero exit code:

| Exit code | Meaning |
| --- | --- |
| `0` | Command succeeded |
| `2` | Usage error |
| `3` | Missing or invalid authentication |
| `4` | Session or resource not found |
| `5` | API or other command failure |
| `6` | Wait timed out |
| `10` | Wait observed `turn_end` |
| `11` | Wait observed `approval` |
| `12` | Wait observed `error` (including an interrupted `turn.failed`) |
| `13` | Wait observed `done` |
| `14` | Plan wait observed `node_started` |
| `15` | Plan wait observed `node_done` |
| `16` | Plan wait observed `failed` |
| `17` | Wait observed a `stalled` running turn |

`status` and session GET expose `last_event_at` (UTC, nullable before provider
output), `idle_seconds` (seconds since provider stdout, or creation before any
output), `active_subagents`, `running_turn`, and `turn_started_at`. Control
traffic and runtime stderr do not refresh the provider clock. An idle maintained
session has `running_turn=false`. `status` reports approvals as unavailable while
the provider transport is attaching, and keeps the durable status readable.

```sh
./agentctl wait SESSION_ID --until stalled --stall 30s --timeout 5m
# Exit 17 observes silence; it does not stop the provider.
./agentctl since SESSION_ID
./agentctl grep SESSION_ID . --type subagent.tool.call --json
```

`--stall` must be positive when `stalled` is selected; it can be combined with
`turn_end,approval,error,done`. Each new prompt starts a fresh silence window.
Provider keep-alives and account/usage notifications count as provider events;
this measures transport silence, not lack of meaningful progress. A sleeping
tool may therefore remain observable without being classified as stalled.
There is no automatic inactivity cutoff; the wall-clock timeout is unchanged.

Child summaries, tools, progress and boundaries use `subagent.*` event types
and payload tags `subagent_thread_id`, `subagent_path`, and `model`. Codex uses
its exposed agent path; Claude uses its task ID and correlates child tool-use
IDs to that task. Unknown models remain an empty string until exposed by the
provider. Metadata is recovered from durable events; child records never bind
the primary generation identity or complete a parent wait. Claude native
streaming requires a CLI supporting `--forward-subagent-text` (qualified on
2.1.302). Child deltas remain durable as `subagent.content.delta` or
`subagent.provider.delta`, available
through `grep --type` or `since --raw`, and are hidden from default `since`
output, including JSON. Human child summaries are labeled `[sub:<path>]`.

For isolated observability acceptance, run
`python3 scripts/agentctl_persist_bootstrap.py claude --observability` and repeat
with `codex`. This mode starts only AgentD, reusing the Node/SQLite ABI preflight,
private data/config homes, forbidden-root/port guards, connection monitoring,
Docker scoping and teardown. It uses the installed local provider CLIs with
private configuration and no host MCP catalog. Each provider spawns one child;
the harness verifies durable labels/type filtering and a count of `0 → 1 → 0`.
For the 30-second silence proof it starts a 90-second tool, pauses only that
test-owned provider PID (to suppress provider heartbeats), observes exit 17,
then resumes and terminates it. Evidence and cleanup receipts stay under the
ignored `.agentctl-live-test/` directory.

Run `scripts/agentctl-live-test.sh claude` and `scripts/agentctl-live-test.sh codex`
for live acceptance. Each builds the current worktree, starts a separate daemon
on 18091/18092, and writes private evidence under ignored `.agentctl-live-test/`.
The test-only Docker proxy labels containers and scopes daemon recovery/pruning
to that run; it maps development image references to existing
`2.3.3-persist.10` images from base `f817cb3`. It changes no global Docker tags.
The isolated credential sync reads the existing provider login and caches only
under the private test data directory. The harness neither uses nor reconfigures
the installed launchd daemon or `~/.agentd`, and stops its daemons/containers.
Requirements: Docker, Python 3, Go, those base images, and working provider logins.
Optional overrides: `AGENTCTL_LIVE_TEST_PORT` and `AGENTCTL_LIVE_TEST_MODEL`.

Steering acceptance requires replacement output and completion after the steer
while the original foreground workload's finish marker remains absent.
Run `go test ./pkg/api -run TestActiveDockerSteerAcrossContinuationModes` for
deterministic initial, woken, and warm-turn coverage using native wire fixtures
at the Docker runtime boundary (local subprocesses, no Docker or provider API).
Run `python3 -m unittest discover -s scripts -p 'test_agentctl*.py'` for harness
regressions, including rejection of naturally completed steering workloads.

Install after review (this command is not run by the tests):

```sh
go build -o "$HOME/.local/bin/agentctl" ./cmd/agentctl
```

Restricted Codex sessions may receive an explicit one-session `auth.json` via
the `AGENTD_CODEX_AUTH_JSON` environment grant. The caller must include that
exact name in `secret_grants`. AgentD validates the JSON, writes it only to the
private ephemeral Codex home, removes it from the provider environment, stores
only the grant name, and destroys the file after the terminal receipt. It never
falls back to host Codex credentials for an explicit-policy session.

## External OpenTraces observer

AgentD supports separately installed trace systems through observer protocol
`1.0`. It sends committed immutable events from the durable ledger and stores
per-adapter acknowledgement checkpoints below `~/.agentd/plugins`. OpenTraces
remains responsible for its own schema, bucket, upgrades, privacy tools, and
remote synchronization; AgentD does not embed or mutate them.

Copy [`plugins.example.json`](plugins.example.json) to
`~/.agentd/plugins.json`, set the absolute path to an externally maintained
OpenTraces adapter, and run `chmod 600 ~/.agentd/plugins.json`. See the
[observer protocol](docs/observer-plugin-protocol.md) for the handshake,
event/ack schema, replay rules, failure policies, and adapter boundary.

Create a session with a caller-owned idempotency key:

```sh
agentd_curl -sS http://127.0.0.1:8090/api/v1/sessions \
  -H 'content-type: application/json' \
  -d '{
    "idempotency_key": "job-2026-08-09-001",
    "agent": "claude",
    "runtime": "docker",
    "prompt": "Inspect the repository and report the failing tests"
  }'
```

The first request returns `201`. Repeating the exact request/key returns the same logical session with `200` and never launches a second paid process. Reusing the key with a changed request returns `409 idempotency_conflict`.

### Millisecond warm prompts and portable resume

Follow-up prompts resume completed native Claude/Codex sessions on local and
Docker runtimes using the stored model, permissions, runtime and provider
context. Keeping a Docker process warm is an optional latency optimization:


```json
{
  "idempotency_key": "conversation-001",
  "agent": "codex",
  "runtime": "docker",
  "prompt": "Inspect the failing test",
  "container_lease": {
    "mode": "maintain",
    "idle_ttl": "10m",
    "portable_resume": true
  }
}
```

After the durable `turn.completed` event, send the next turn to
`POST /api/v1/sessions/{session_id}/input` with `kind: "prompt"`. During an
active turn use `kind: "steer"`. Docker and provider bootstrap are absent from
the warm-prompt path; the independent per-turn timeout is reset for each new
prompt. Idle expiry closes the transport and seals that generation's receipt;
a later prompt wakes the next generation. Explicit termination prevents further
continuation. Receipts and diagnostic output remain immutable per generation.
Use `GET /api/v1/sessions/{id}/receipt?generation=N` or `/result?generation=N`
for historical proof; the default selects the active generation. See the
[continuation contract](docs/specs/agentd-durable-streaming/CONTINUATION.md) for
state transitions, schema v4 migration and unavailable-state reasons.

`POST /api/v1/sessions/{session_id}/resume-state` creates a content-addressed
portable snapshot while a maintained session is idle or after it is terminal.
Use `GET` on that path for the latest snapshot, download it from
`GET /api/v1/resume-states/{resume_state_id}`, or upload an `.agentstate` file
to `POST /api/v1/resume-states`. A new Docker call cold-imports it with
`resume_state_id`; its `work_dir`, mounts, and secret grants come from the new
request and may differ from the source host. Portable state contains provider
conversation files and provenance only, not credentials or workspace data.

Stopped AgentD containers are removed at terminal finalization. A supervised
cleanup pass also removes old stopped AgentD-labeled containers left by crashes
or prior releases while preserving named provider-state volumes.

The response includes:

```json
{
  "api_version": "v1",
  "data": {
    "session_id": "...",
    "idempotency_key": "job-2026-08-09-001",
    "agent": "claude",
    "runtime": "docker",
    "state": "running",
    "generation": 1,
    "last_sequence": 0,
    "events_url": "http://.../api/v1/sessions/.../events",
    "event_stream_url": "ws://.../api/v1/ws/sessions/.../events"
  }
}
```

### Replay and live streaming

Page committed events strictly after a sequence:

```sh
agentd_curl -sS 'http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/events?after_sequence=42&limit=100'
```

Continue stored-then-live delivery without a replay/live race:

```sh
websocat 'ws://127.0.0.1:8090/api/v1/ws/sessions/SESSION_ID/events?after_sequence=42'
```

Each event has a stable schema version, ID, session ID, generation, sequence, timestamp, type, stream, derived payload, exact raw bytes (`raw_base64`), and raw SHA-256 hash.

The first WebSocket frame is `stream.ready` and identifies the durable replay boundary. Slow consumers reconnect from their last contiguous sequence; they never block provider ingestion.

### Follow-up, steer, interrupt, cancel, terminate, and resume

Every state-changing control requires its own idempotency key:

```sh
agentd_curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/input \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"input-001","kind":"prompt","text":"Now apply the fix"}'

agentd_curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/interrupt \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"interrupt-001"}'

agentd_curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/cancel \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"cancel-001"}'
```

Administrative forced termination is separate from caller cancellation:

```bash
agentd_curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/terminate \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"terminate-001"}'
```

Both close the logical session, but the immutable receipt/event reason is
`cancelled` or `terminated`, respectively.

`resume` creates generation `N+1` only for an eligible nonterminal session whose prior runtime generation is durably `lost`. Reconnect/replay of an existing generation does not call `resume`.

```sh
agentd_curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/resume \
  -H 'content-type: application/json' \
  -H 'Idempotency-Key: resume-001' \
  -d '{"prompt":"Continue after runtime loss"}'
```

Terminal proof is available after completion:

```sh
agentd_curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/receipt
```

### CLI attach

```sh
agentd attach SESSION_ID --since 42
```

`--since` is a durable event sequence. `--no-replay` snapshots the current durable tail and shows only later events. Interactive lines become v1 follow-up prompts; `/steer ...` and `/interrupt` use durable controls. Ctrl+C interrupts once and detaches on the second press.

## Named chats

Named chats persist their JSON records and session chain under the AgentD data root. Production Claude/Codex chat spawns use the same durable v1 generation and control ledger as direct sessions.

```sh
agentd chat create project-review --agent claude --runtime docker
agentd chat send project-review 'Review the current branch'
agentd chat attach project-review
agentd chat list
```

## Data layout

AgentD defaults to `~/.agentd`:

```text
~/.agentd/
  agentd.sqlite       durable sessions, generations, events, receipts
  backups/            verified SQLite backup manifests/artifacts
  chats/              named-chat JSON records
  logs/               diagnostic NDJSON mirrors and output hashes
  sessions/           reconstructable materialized session files
  credentials/        AgentD-managed credential copies when enabled
```

The SQLite store is authoritative for v1 replay. Compatibility logs and in-memory replay buffers are not accepted as proof of a complete durable event history.

Never place secret values in durable request metadata. Environment secrets must be explicitly named in `secret_grants`; values are launch-only and excluded from the reconstructable request manifest and logs.

## Docker reconstruction

Durable containers are labeled with logical session ID, idempotency key, request hash, generation, agent, image reference, immutable image digest, and sandbox-profile version.

During startup AgentD:

1. discovers running and stopped labeled containers;
2. rejects duplicate generation claims as `indeterminate`;
3. reattaches the exact expected generation;
4. reconciles the retained native output prefix against committed event hashes;
5. marks confirmed missing generations `lost`; and
6. reconstructs a pre-commit container only when every durable admission label matches.

`docker wait` must agree with inspected container state before terminal proof is trusted. Proven policy-limited OOM kills are recorded as typed `resource_limit_exceeded` failures with exit 137 and `SIGKILL`. Ambiguous recovery is explicit `indeterminate`—AgentD does not silently restart paid work.

Controlled shutdown closes admission first, drains bounded local work, and preserves active Docker generations for the next daemon to reconstruct.

The authenticated capability response makes this boundary machine-readable as
`recovery.version: "1.0"`. `daemon_restart` is `docker_only` only when durable
replay and Docker reconstruction are both available; `supported_runtimes` then
contains only `docker`, while `local` is listed in `unsupported_runtimes`.
Without that full proof surface, recovery advertises `unsupported`. Callers
must gate restart-recoverable work on this versioned object rather than infer
support from the general runtime list or replay persistence.

## Development and qualification

```sh
go test ./...
go test -race ./...
go vet ./...
```

Run the deterministic 30-session process-boundary scenario:

```sh
go test -tags='e2e concurrency' -timeout=300s ./pkg/e2e \
  -run TestConcurrency_30Sessions -count=1 -v
```

The scenario requires exactly 30 completed durable receipts and retains a
private `0700` run directory under `.artifacts/concurrency/` containing the
redacted environment, per-session results, process/FD/RSS/latency samples, and
`0600` daemon log. Set `AGENTRUNTIME_CONCURRENCY_ARTIFACT_DIR` to choose a
different private artifact directory.

Session diagnostic logs are non-canonical mirrors: canonical events and replay
remain byte-exact. Diagnostics default to enabled with seven-day retention,
owner-only `0700` directories / `0600` files, and prompt/credential redaction.
Configure once at startup with either CLI flags or higher-priority environment
variables:

```sh
agentd --diagnostic-logs=false
agentd --diagnostic-log-retention=24h
AGENTD_DIAGNOSTIC_LOGS=true AGENTD_DIAGNOSTIC_LOG_RETENTION=168h agentd
```

A retention of `0` keeps diagnostic files indefinitely. Disabling diagnostics
creates no session log directory or file. The durable event ledger, results,
receipts, and replay contract are unaffected.

Prove that the installed artifact is the exact release under qualification:

```sh
agentd --build-info
agentd --require-build '2.1.0@FULL_RELEASE_COMMIT_SHA'
```

The second command exits nonzero for a version mismatch, commit mismatch, or an
artifact without verifiable source identity. Release builds inject both values;
capabilities expose the same pair to remote callers.

Run the direct Docker attach/reconnect proof:

```sh
AGENTRUNTIME_DOCKER_INTEGRATION=1 \
AGENTRUNTIME_DOCKER_TEST_IMAGE=alpine:3.20 \
go test ./pkg/runtime \
  -run TestDockerDirectAttach_ReconnectsStdinAndRecoversOrderedOutput \
  -count=1 -v
```

The canonical implementation tracker is [docs/specs/agentd-durable-streaming/TASKS.md](docs/specs/agentd-durable-streaming/TASKS.md). The direct-native transport decision is documented in [ADR-001](docs/specs/agentd-durable-streaming/ADR-001-native-docker-transport.md).

## Compatibility status

The execution sidecar and its port/health/WebSocket protocol are gone. The CLI attach/dispatch flows, TUI, embedded dashboard, typed Go client, and chat history use durable v1 sequences. The typed client covers dispatch, list/inspect, replay/raw streaming, prompt/steer, interrupt, cancel, terminate, lost-generation resume, portable state transfer, and immutable receipts. Unversioned session, byte-log, and bidirectional bridge routes have been removed. Pre-v1 chat NDJSON remains read-only and is explicitly reported as unverified legacy history.

AgentD's private v1 HTTP and WebSocket surfaces require the token stored below
its data root. The health-only public surface contains no session or credential
data. Non-loopback binding is explicit and does not weaken authentication.

## License

MIT. See [LICENSE](LICENSE).

### Isolated PERSIST acceptance

Run `scripts/agentctl-persist-live-test.sh claude` and then the same command
with `codex`. Each run builds its own binaries and creates private data/config
roots, credentials, cursors, Docker networks and proxy. `--flows a-local,a-docker,b,c,d`
selects scenarios (all are required for qualification); `--preflight` checks Node
and SQLite without starting services. Evidence and cleanup receipts are under
`.agentctl-live-test/persist-PROVIDER/run.*`.

The runner honors the PERSIST checkout's `.node-version`/`.nvmrc`, fails before
startup on a native SQLite ABI mismatch, sets `PERSIST_HTTP__PORT`, explicitly
enables `agentruntime.clean_context` for local and Docker requests, and uses
`/health` for unauthenticated readiness. It binds the CLIs through isolated URLs
and private token files. Node processes reject attempts to use ports 8803/38093
or the prohibited production roots. No installed services are restarted.

Raw task-owned session controls require PERSIST authorization; a hidden task
cannot fall back to a direct write. Raw session IDs also undergo PERSIST ownership
lookup when their AgentD manifest has no task ID. Standalone sessions use direct
control after a negative ownership lookup; lookup/authentication failures block
writes. Use `--direct` before the session ID to deliberately take over through
AgentD, including when PERSIST is unavailable.
