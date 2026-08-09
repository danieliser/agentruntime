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
--port 8090
--runtime local|docker
--data-dir ~/.agentd
--docker-host ssh://user@host
--max-sessions 0
--credential-sync
```

`AGENTRUNTIME_DATA_DIR` overrides the complete data root. `--data-dir` takes precedence when explicitly supplied.

## Durable v1 session API

Check compatibility before submitting work:

```sh
curl -sS http://127.0.0.1:8090/api/v1/capabilities
```

The handshake reports the AgentD/API/event-schema versions, native providers, configured runtimes, replay persistence, Docker reconstruction, and plugin API versions. An empty plugin-version list means no plugin contract is currently available.

Create a session with a caller-owned idempotency key:

```sh
curl -sS http://127.0.0.1:8090/api/v1/sessions \
  -H 'content-type: application/json' \
  -d '{
    "idempotency_key": "job-2026-08-09-001",
    "agent": "claude",
    "runtime": "docker",
    "prompt": "Inspect the repository and report the failing tests"
  }'
```

The first request returns `201`. Repeating the exact request/key returns the same logical session with `200` and never launches a second paid process. Reusing the key with a changed request returns `409 idempotency_conflict`.

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
curl -sS 'http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/events?after_sequence=42&limit=100'
```

Continue stored-then-live delivery without a replay/live race:

```sh
websocat 'ws://127.0.0.1:8090/api/v1/ws/sessions/SESSION_ID/events?after_sequence=42'
```

Each event has a stable schema version, ID, session ID, generation, sequence, timestamp, type, stream, derived payload, exact raw bytes (`raw_base64`), and raw SHA-256 hash.

The first WebSocket frame is `stream.ready` and identifies the durable replay boundary. Slow consumers reconnect from their last contiguous sequence; they never block provider ingestion.

### Follow-up, steer, interrupt, cancel, and resume

Every state-changing control requires its own idempotency key:

```sh
curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/input \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"input-001","kind":"prompt","text":"Now apply the fix"}'

curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/interrupt \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"interrupt-001"}'

curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/cancel \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"cancel-001"}'
```

`resume` creates generation `N+1` only for an eligible nonterminal session whose prior runtime generation is durably `lost`. Reconnect/replay of an existing generation does not call `resume`.

```sh
curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/resume \
  -H 'content-type: application/json' \
  -d '{"prompt":"Continue after runtime loss"}'
```

Terminal proof is available after completion:

```sh
curl -sS http://127.0.0.1:8090/api/v1/sessions/SESSION_ID/receipt
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
  logs/               compatibility NDJSON logs and output hashes
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

`docker wait` must agree with inspected container state before terminal proof is trusted. Proven OOM kills are recorded as `crashed` with exit 137 and `SIGKILL`. Ambiguous recovery is explicit `indeterminate`—AgentD does not silently restart paid work.

Controlled shutdown closes admission first, drains bounded local work, and preserves active Docker generations for the next daemon to reconstruct.

## Development and qualification

```sh
go test ./...
go test -race ./pkg/api ./pkg/eventstream ./pkg/runtime
go vet ./...
```

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

The execution sidecar and its port/health/WebSocket protocol are gone. The CLI attach/dispatch flows, TUI, embedded dashboard, and typed Go client use durable v1 sequences. Legacy Go-client methods, unversioned daemon routes, and server chat-log pagination remain temporarily during migration; their byte offsets are a separate cursor domain and must not be treated as v1 event sequences.

AgentD currently has no API authentication layer. Bind it to a trusted interface or place it behind authenticated transport until the versioned security follow-up lands.

## License

MIT. See [LICENSE](LICENSE).
