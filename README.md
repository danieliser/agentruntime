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

An unrestricted native Docker session can keep its provider process warm
between turns:

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
prompt. Idle expiry or explicit termination closes the transport and creates
the one terminal receipt for the logical conversation.

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
