# AgentD Durable Native Streaming — Task Sheet

Status: **G0 + G1 + G2 + G3 + G4 + G5 + G6 APPROVED / PHASE 10 QUALIFIED / v2.1.1 HOTFIX IN PROGRESS**

Last updated: 2026-08-10

Primary scope: Claude + Codex JSON streaming, durable replay, idempotent resume, Docker reconstruction, external trace observers

## 1. Adjusted capability-gap list

This list is first by design. It supersedes broader interpretations of the earlier runtime review.

| # | Area | Current position | Required change |
|---|---|---|---|
| 1 | Native streaming transport | Claude stream-json and Codex app-server JSON-RPC patterns are already exposed and working inside the adapters. AgentD currently routes them through a second sidecar WebSocket/event/replay protocol. | Make provider-native JSON the canonical input/output. Retire the sidecar semantic protocol. Prove whether Docker can support direct durable attach; retain only a minimal transparent stdio owner if reattach requires one. |
| 2 | Buffered chat replay | Current output is tee'd into a bounded byte ring and append-only NDJSON, with byte-offset replay. The ring may silently advance stale offsets and is not the durable authority. | Persist every committed event before publishing it. Replay from a stable per-session sequence pointer, then continue live without gaps or identity changes. |
| 3 | Create/resume behavior | Caller `session_id` exists, but duplicate create returns `409`. Provider resume exists on separate paths and session metadata is mostly in memory. | Same idempotency key + same request returns the existing logical session. Reconnect active sessions; reconstruct/resume recoverable Docker sessions. Reserve `409` for key reuse with a different request hash. |
| 4 | Event protocol | Sidecar events are partially normalized and have timestamp/byte offset, but no schema version, stable event ID, logical sequence, generation, or guaranteed raw preservation. | Wrap canonical native records in one AgentD envelope with stable IDs/sequences. Preserve exact provider JSON and expose derived event type/payload without replacing the raw record. |
| 5 | Docker durability | Running labeled containers can be rediscovered as `orphaned`, but the durable job/session/generation record is missing and replay may duplicate or truncate on recovery. | Persist logical sessions and runtime generations, label containers with them, reconcile DB↔Docker on startup, and reattach/replay exactly once from the durable event boundary. |
| 6 | Terminal receipt | Exit code/timestamps exist in memory and logs, but there is no durable immutable receipt. | Persist terminal reason, code/signal, timestamps, last sequence, provider session/thread ID, and hashes. Terminal logical sessions never return to running. |
| 7 | Lifecycle/shutdown | Start, inspect, stream, WS interrupt, delete/kill, and provider continuation exist. Shutdown currently kills sessions immediately. | Add idempotent control operations and stop admission before a bounded drain. Runtime generation replacement must be explicit. |
| 8 | Version handshake/API security | `/health` exposes build version and runtime names; public routes are unversioned and unauthenticated. | Track as a follow-up epic. This sheet only reserves schema/API version fields needed by durable streaming. |
| 9 | Sandbox hardening | Docker has a non-root default image, cap-drop/no-new-privileges, optional CPU/memory, clean context, and proxy routing. | Track separately. Do not pull broad sandbox redesign into the streaming/durability critical path. Record runtime/image identity needed for reconstruction. |
| 10 | Plugins/OpenTraces | The durable event ledger/replay contract is qualified, but no execution-observer plugin API exists. | Add a versioned, allowlisted, replay-safe observer protocol and an OpenTraces adapter boundary. OpenTraces remains separately installed and maintained; AgentD must not own its schema, bucket, upgrade, or synchronization lifecycle. |
| 11 | Retry ownership | AgentD classifies retryability but does not retry complete paid sessions. | No architecture change. DBOS/Trading Floor retains workflow retry authority. |

## Working rules — do not drift

1. This file is the canonical task sheet for this effort.
2. During planning, **only this file may be added or updated**. Do not change code, schemas, migrations, APIs, generated files, or dependencies.
3. During implementation, every code change must cite a task ID from this sheet.
4. Tasks are append-only: update status/evidence or add an explicitly approved task; never delete or silently broaden one.
5. Scope changes require explicit user approval and a dated entry in **Scope change log**.
6. Red–Green–Refactor is mandatory. Each behavior task starts with a failing deterministic test.
7. No database migration file may be created until Gate G1 (schema review) is approved.
8. No current transport may be removed until Gate G2 (native-stream parity) is approved.

## 2. Goal and ownership boundary

AgentD owns:

- one logical execution session identity;
- one or more explicit Docker runtime generations for that session;
- native Claude/Codex input and output transport;
- durable event ingestion, buffering, replay, and terminal receipts;
- inspect, reconnect, interrupt, cancel, and reconstruction mechanics.

Trading Floor/DBOS owns:

- workflow/job orchestration and retry decisions;
- approvals, budgets, policy, and scheduling;
- final artifact admission and product authority;
- deciding whether to create a new logical session after a terminal result.

### In scope

- Claude stream-json input/output and Codex app-server JSON-RPC.
- AgentD-owned versioned event envelope and append-only event persistence.
- Cursor-based replay for chat/output clients.
- Idempotent create/lookup and explicit runtime resume/reconstruction.
- Durable Docker session/generation metadata and startup reconciliation.
- Minimal terminal receipt/state-machine work required for honest reconstruction.
- Compatibility path for current clients while the old sidecar transport is retired.
- Versioned external observer delivery with durable replay/checkpoint semantics.
- A separately executed OpenTraces adapter contract, trace linkage, and health reporting.

### Out of scope for this sheet

- DBOS workflows, retries, approvals, or artifact admission.
- OpenTraces internals: its trace schema, buckets, storage migrations, upgrade lifecycle, and remote synchronization.
- Model-installed/dynamic plugins or plugins with execution-control authority.
- Broad API authentication rollout or full sandbox redesign.
- Grok/Cursor native transport parity.
- Durable local-process recovery; Docker is the qualification boundary.
- New migration files before schema review approval.

## 3. Locked design decisions

These decisions may only change through an approved scope-log entry.

### D1 — Native provider records are canonical

- Claude stream-json records and Codex JSON-RPC messages are stored exactly as received.
- AgentD may derive `content`, `tool_call`, `tool_result`, `usage`, `error`, `lifecycle`, and `terminal` views.
- Derived data never replaces the exact raw JSON record.
- Runtime stderr is a separate event stream; it is never interleaved into provider JSON.

### D2 — AgentD owns the only public event envelope

Minimum envelope:

```json
{
  "schema_version": "1.0",
  "event_id": "uuid",
  "session_id": "uuid",
  "generation": 1,
  "sequence": 42,
  "timestamp": "RFC3339Nano",
  "type": "content.delta",
  "stream": "provider_stdout",
  "payload": {},
  "raw": {}
}
```

- Sequence is monotonic and contiguous per logical session, across runtime generations.
- An event ID and sequence are assigned once, in the same transaction that persists the event.
- Publish happens only after commit.
- Delivery is at-least-once; stable `(session_id, sequence, event_id)` makes replay idempotent.
- A cursor means “highest contiguous sequence durably processed by the client.”

### D3 — The durable store is authoritative

- Use a single AgentD-owned SQLite store for logical sessions, runtime generations, events, and terminal receipts.
- In-memory buffers are acceleration only; they are never the recovery authority.
- Event rows are append-only.
- Terminal receipts are immutable once committed.
- Schema design is reviewed before any migration is written.

### D4 — Duplicate create is lookup, not restart

- `POST /api/v1/sessions` accepts an idempotency key/job ID and a canonical request hash.
- First request: `201 Created` with the new logical session.
- Same key + same request hash: `200 OK` with the existing session, current state, replay cursor, and connection information.
- Same key + different request hash: `409 idempotency_conflict`.
- Repeating create never launches a second paid model process.
- A terminal session is returned as terminal; it is not revived.

### D5 — Resume and reconnect are separate

- **Reconnect:** attach a client to the existing generation and replay events after its cursor.
- **Reconstruct:** AgentD restart discovers the same running Docker generation and resumes event ingestion.
- **Resume runtime:** if a nonterminal logical session lost its runtime but provider state is recoverable, create generation `N+1` under the same logical session.
- **Continue after terminal:** requires a new logical session/idempotency key unless a later explicitly approved contract says otherwise.

### D6 — Direct Docker transport; retire sidecar semantics

Gate G0 selected direct Docker create/start/attach with durable stdin
reattachment and full retained-log prefix reconciliation. Remove the sidecar
WebSocket normalization/replay/control protocol after Gate G2 native parity.
No replacement in-container transport owner is planned.

### D7 — OpenTraces is an external immutable-event observer

- AgentD exposes one generic observer plugin protocol; it does not embed or fork OpenTraces.
- OpenTraces is installed, configured, upgraded, and operated independently from AgentD.
- The adapter consumes complete immutable AgentD v1 event envelopes and cannot steer, interrupt, cancel, mutate, authorize, or admit a session.
- AgentD's durable event ledger is the replay buffer. Per-plugin/session checkpoints advance only after an acknowledgement of the exact event ID and sequence.
- Plugin delivery is at-least-once. Stable event identity plus adapter idempotency prevents duplicate trace effects after either process restarts.
- Plugins are explicitly allowlisted in local configuration. AgentD never accepts model-installed or dynamically proposed plugins.
- The default trace policy is `best_effort`. A caller-selected `required` policy may reject admission when the configured adapter is absent, unhealthy, or incompatible, but a plugin never gains runtime control after admission.
- Local delivery is the default. AgentD neither invokes nor authorizes OpenTraces remote synchronization and never logs secret values.
- Plugin state, checkpoints, and local spool files live below the unified AgentD data root (`~/.agentd/plugins` by default).

## 4. Lifecycle model

Logical session states:

```text
created → starting → running → completed
                    ↘ failed
                    ↘ cancelled
                    ↘ timed_out
                    ↘ crashed
                    ↘ indeterminate
```

- Terminal states are immutable.
- `interrupt` affects the current turn but does not terminate the logical session.
- `cancel` requests terminal `cancelled` and is idempotent.
- Runtime generation replacement never changes the logical session ID or resets event sequence.
- If AgentD cannot prove whether unrecorded output/work occurred, it records `indeterminate`; it does not guess success or silently restart.

## 5. Public replay contract

Planned endpoints; exact naming is finalized at Gate G1:

```text
POST /api/v1/sessions
GET  /api/v1/sessions/{session_id}
GET  /api/v1/sessions/{session_id}/events?after_sequence=N&limit=M
GET  /api/v1/sessions/{session_id}/events/stream?after_sequence=N
POST /api/v1/sessions/{session_id}/interrupt
POST /api/v1/sessions/{session_id}/cancel
POST /api/v1/sessions/{session_id}/resume
```

Replay behavior:

1. Read and send stored events strictly after `after_sequence`.
2. Establish the live subscription without a replay/live race.
3. Send newly committed events in sequence order.
4. Replayed events retain their original IDs, sequences, timestamps, generation, raw record, and payload.
5. Invalid future cursor returns a structured error.
6. Missing/corrupt sequence returns explicit `event_gap` or `indeterminate`; never silently advances.
7. A slow/disconnected client never blocks provider ingestion; it reconnects from its last contiguous sequence.

## 6. Work breakdown

Statuses: `TODO`, `IN PROGRESS`, `BLOCKED`, `DONE`. Evidence is required for `DONE`.

### Phase 0 — Transport proof and ADR

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| STR-001 | DONE | Capture scrubbed Claude stream-json and Codex app-server fixtures for parser/transport tests. Keep paid live probes opt-in. | `testdata/native-streams/`; fixture validation and coverage tests pass. | S |
| STR-002 | DONE | Spike direct Docker `create/start/attach` with open stdin and daemon detach/reattach for both providers. | Real-Docker integration test proves input reattach without container/process restart; native fixtures cover both provider transports. | M |
| STR-003 | DONE | Test Docker log/attach ordering, stdout/stderr separation, and recovery boundaries under forced disconnect. | Integration test proves ordered retained stdout, separate stderr, and timestamped recovery records; prefix-loss boundary is explicit in ADR-001. | M |
| STR-004 | DONE | Write ADR choosing direct attach or minimal transparent transport owner. | `ADR-001-native-docker-transport.md` chooses direct attach and contains the old-sidecar removal map. | S |

**Gate G0 — APPROVED 2026-08-09:** direct Docker native JSON transport; no replacement sidecar broker.

### Phase 1 — Durable contracts and store

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| DUR-101 | DONE | Define typed repository interfaces and structured domain errors for sessions, generations, events, and receipts. | In-memory and SQLite implementations pass one shared store contract. | M |
| DUR-102 | DONE | Design SQLite schema, constraints, request hashing, transaction boundaries, backup/restore, and append-only protections. | Approved migration, private filesystem modes, integrity checks, snapshots, and hashed JSON backup manifests pass. | M |
| DUR-103 | DONE | Implement idempotent logical-session creation and lookup transaction. | SQLite passes 32-way concurrent duplicate-create, request-hash conflict, and restart lookup tests. | M |
| DUR-104 | DONE | Implement atomic sequence allocation + event append. | SQLite passes 100-way contiguous allocation, stable event-ID, replay, deletion-gap, and raw-hash corruption tests. | L |
| DUR-105 | DONE | Implement immutable state transitions and terminal receipt persistence. | Atomic generation/session/receipt finalization remains immutable and identical after database restart. | M |

**Gate G1 — APPROVED 2026-08-09:** four-table SQLite schema, typed store contracts, append-only enforcement, and backup/restore design.

### Phase 2 — Native Claude/Codex ingestion

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| NAT-201 | DONE | Add one canonical native transport interface for start, input, interrupt, output records, stderr, wait, and recovery metadata. | `pkg/nativeprotocol`; Claude and Codex pass the same transport contract, including observable stream-read failure. | M |
| NAT-202 | DONE | Route Claude native stream-json input/output directly into AgentD ingestion. | Durable Claude generations retain their full native command, use direct Docker attach/logs/wait, and commit byte-exact fixture output plus terminal state. | L |
| NAT-203 | DONE | Route Codex app-server JSON-RPC directly into AgentD ingestion. | Durable Codex generations launch app-server over stdio, perform initialize plus thread start/resume and turn start, and persist each exact RPC record directly. | L |
| NAT-204 | DONE | Add envelope derivation without raw-record loss. | `pkg/eventstream`; source-position IDs, exact raw bytes/hash, stable envelope, and derived fixture types pass. | M |
| NAT-205 | DONE | Separate provider stdout, runtime stderr, lifecycle, control acknowledgment, and terminal events. | Native provider/RPC records, runtime stderr, derived lifecycle/control types, and the stable terminal event are committed to distinct stream classes; receipts include the terminal sequence. | M |

**Gate G2 — Native-stream parity:** Claude/Codex streaming, control, resume, usage, and terminal parity must pass before old sidecar semantics are removed.

**Gate G2 — APPROVED 2026-08-09:** Claude/Codex native streaming,
follow-up/steer, interrupt, cancel, resume, terminal proof, usage derivation, and
cursor replay pass without the execution-sidecar protocol. The user approved
retirement of non-native methods after this parity checkpoint.

### Phase 3 — Buffered storage and cursor replay

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| REP-301 | DONE | Build commit-before-publish event fanout over the durable store. | Post-commit fault injection replays the committed event once with the same identity; slow subscribers cannot block commits. | L |
| REP-302 | DONE | Add paginated replay API using `after_sequence`. | Versioned API passes zero/middle/latest/future, terminal, raw-byte, 1,001-event history, limit-cap, and pagination tests. | M |
| REP-303 | DONE | Add live stream handshake that closes the replay/live race. | WebSocket `stream.ready` snapshots the durable tail; forced replay-to-live event appears exactly once. | L |
| REP-304 | DONE | Detect sequence corruption/missing rows and return `event_gap`/`indeterminate`. | SQLite gap/hash corruption tests and subscription contiguity checks reject silent advancement. | M |
| REP-305 | DONE | Define retention/archive policy with no silent truncation. | Event protocol v1 exposes earliest/tail; v1 retains the complete ledger and forbids silent truncation. | M |

### Phase 4 — Idempotent reconnect and resume

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| RES-401 | DONE | Replace duplicate-create `409` with idempotent lookup semantics. | Versioned create passes 16-way concurrent admission with one process/generation, changed-request conflict, terminal lookup, and SQLite close/reopen lookup without respawn. | M |
| RES-402 | DONE | Persist provider session/thread identity and resume inputs. | Provider identity binds from native output, survives SQLite restart, restores Codex correlation without repeating app-server initialization, and is supplied exactly to Claude/Codex generation resume. | M |
| RES-403 | DONE | Implement explicit runtime resume as generation `N+1` for eligible nonterminal sessions. | `POST /api/v1/sessions/{id}/resume` accepts only a lost nonterminal generation, retains logical ID/event sequence, creates N+1 once, resumes the exact provider ID, and returns lookup/no-op on repeats or terminal sessions. | L |
| RES-404 | DONE | Make interrupt/cancel/terminate/resume requests idempotent with durable outcomes. | Resume is serialized/idempotent; prompt/steer/interrupt/cancel/terminate require durable keys and commit `requested` then `dispatched`. Cancellation and administrative termination wait for dispatch proof before distinct `session.cancelled`/`session.terminated` receipt reasons; stable retries are no-ops and ambiguous dispatch is explicit `indeterminate`. | M |
| RES-405 | DONE | Expose typed follow-up and steer input on an active native generation. | `POST /api/v1/sessions/{id}/input` routes prompt/steer through the registered Claude/Codex native transport; interactive Codex integration proves a second turn and interrupt without a sidecar. | M |

### Phase 5 — Docker reconstruction

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| DKR-501 | DONE | Persist and label session ID, job key, request hash, generation, container ID, image reference/digest, and sandbox-profile version. | Durable Docker admission resolves the image reference to an immutable `sha256:` ID before launch; labels session/job/hash/generation/image reference/digest/sandbox profile; verifies the created container used that exact digest; and persists the same identity with fresh and resumed generations. | M |
| DKR-502 | DONE | Implement startup reconciliation across expected/running/exited/missing/duplicate containers. | Expected generations reattach, confirmed-missing generations become `lost`, duplicate claims become terminal `indeterminate`, and stopped labeled containers are discovered. Initial and explicit replacement containers created before their generation transaction are reconstructed only from an exact durable admission/label match; mismatches terminate `indeterminate`. Terminal Docker state is inspected and must agree with `docker wait`. | L |
| DKR-503 | DONE | Reattach native input/output at the last durable boundary. | Fresh and recovered durable Docker handles use reattachable stdin plus retained logs directly and never query the sidecar port. Recovery deduplicates the retained prefix; crash-before-bind Codex transport learns its thread ID from retained native output without repeating app-server initialization. | L |
| DKR-504 | DONE | Recover terminal state when container exits while AgentD is down. | Docker recovery includes stopped containers, retained logs, `docker wait`, and matching `.State` inspection. Proven OOM kills persist `crashed`, `SIGKILL`, exit 137, and runtime timestamps. Cancel and timeout remain distinct through durable control proof. Because Docker does not durably prove arbitrary non-OOM signals, signal-shaped `128+N` exits without positive proof become `indeterminate` with no invented signal. | M |
| DKR-505 | DONE | Add admission stop + bounded drain before daemon shutdown. | Admission is closed under a shared gate before HTTP shutdown; local work drains until the caller deadline and is then stopped, while active Docker generations and their runtime infrastructure are preserved for reconstruction. Repeated race tests prove post-gate requests are rejected and Docker handles are not killed. | M |

**Gate G3 — Docker durability qualification:** all restart/replay/resume tests pass against a real Docker daemon before declaring the session contract durable.

### Phase 6 — Compatibility and retirement

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| CMP-601 | DONE | Add a compatibility adapter for current clients during v1 rollout. | `agentd attach`, `agentd dispatch`, the TUI, embedded dashboard, Go client, named chats, and chat history consume v1 sequence events and durable controls. After all first-party consumers moved, the compatibility Go methods and unversioned session/WS/log routes were deleted. | M |
| CMP-602 | DONE | Decide treatment of legacy NDJSON logs: import, read-only history, or explicit legacy status. | Legacy NDJSON remains read-only for pre-v1 chat history and every projected entry is labeled `legacy_ndjson_unverified`; it is never imported or presented as a complete v1 ledger. New chat history derives from immutable v1 events and is labeled `durable_v1`. | M |
| CMP-603 | DONE | Remove sidecar WS normalization/replay after Gate G2. | The sidecar binary, local sidecar runtime, Docker health/port/WS path, WS runtime handle, sidecar image build, non-native Grok/Cursor registration, daemon bridge package, raw-handle chat input, and byte-cursor compatibility routes are removed. | M |
| CMP-604 | DONE | Update docs, examples, SDK, and capability response for the v1 stream contract. | README, architecture, and contributor guidance describe native Claude/Codex, durable v1 sequences/controls/receipts, `~/.agentd`, Docker reconstruction, and the compatibility boundary. The Go client has typed durable session/list/event/capability methods and stable one-shot dispatch keys. `/api/v1/capabilities` reports AgentD/API/event-schema/provider/runtime/replay/reconstruction/plugin compatibility. The embedded dashboard and TUI use durable list/inspect/replay/control surfaces; the redundant standalone dashboard is removed. | M |

### Phase 7 — External observers and OpenTraces

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| PLG-701 | DONE | Define observer plugin API v1 over NDJSON stdio: version/capability handshake, immutable event delivery, exact acknowledgements, flush, health, and graceful shutdown. | `pkg/observer` contract tests reject incompatible API/event schemas, missing trace/idempotency capabilities, and acknowledgements with the wrong delivery/session/event/sequence identity. Supported messages contain no execution controls. | M |
| PLG-702 | DONE | Add explicit allowlisted plugin configuration under the AgentD data root, with sanitized environment grants and `best_effort`/`required` policy. | `plugins.example.json`; config tests require an absolute executable, private file mode, safe names, unique allowlist entries, explicit clean environment, and valid policies. API tests prove required first-admission rejection and idempotent lookup preservation. | M |
| PLG-703 | DONE | Supervise external observer processes without coupling their lifecycle to provider execution. | Subprocess tests cover clean environment, active health, bounded slow response, bad acknowledgement, missing/incompatible executable, crash, flush, and shutdown. Durable append remains available while the adapter is down. | L |
| PLG-704 | DONE | Deliver committed events from the durable ledger and persist atomic per-plugin/session acknowledgement checkpoints. | Manager tests prove ledger scan plus nonblocking commit wakeup, atomic private checkpoints, restart replay, future/corrupt identity rejection, and accepted-before-checkpoint crash recovery through duplicate acknowledgement. | L |
| PLG-705 | DONE | Implement the OpenTraces adapter boundary and deterministic Trading Floor job → AgentD session/generation → OpenTraces trace linkage without owning OpenTraces storage. | External adapter fixture receives exact events plus scrubbed job/session/provider/image/sandbox context, returns a stable UUID trace ID, deduplicates replay, and upgrades independently. AgentD writes no OpenTraces bucket/schema state. | L |
| PLG-706 | DONE | Expose configured plugin capabilities, compatibility, linkage, lag, and healthy/degraded/down state through versioned API/SDK surfaces. | `/api/v1/capabilities`, `/api/v1/plugins`, and `/api/v1/sessions/{id}/traces` plus typed Go-client methods expose API version, adapter version/policy/state/error/lag, and stable acknowledged linkage. | M |
| PLG-707 | DONE | Document configuration, failure policies, privacy boundary, external OpenTraces ownership, and operational recovery. | README, `plugins.example.json`, and `docs/observer-plugin-protocol.md` document local stdio, clean environment, policies, `~/.agentd/plugins`, replay, upgrades, and the external OpenTraces ownership/privacy boundary. | S |

**Gate G4 — APPROVED 2026-08-09:** user approved OpenTraces scope and clarified that the traces system remains externally maintained. AgentD owns only the generic observer protocol, replay/checkpoint delivery, supervision, linkage, and health boundary.

### Phase 8 — External OpenTraces implementation and global setup

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| OTR-801 | DONE | Install or upgrade OpenTraces globally with global tracking, Claude/Codex observational hooks, git correlation, and the shared skill. Keep capture local-only unless the user separately authorizes authentication/remote sync. | Editable pipx install reports OpenTraces `0.4.13` / schema `0.9.0`; doctor verifies global Claude/Codex/git/skill/watcher setup; bucket sync status is `unconfigured` with `remote_unconfigured`. | M |
| OTR-802 | DONE | Establish the AgentD adapter in an independently maintained OpenTraces checkout/package and select its supported capture/ingestion seam from current upstream contracts. | Independent `/Users/danieliser/Toolkit/opentraces` checkout implements registered `AgentDSessionParser` plus the public `ingest_one_session` pipeline; AgentD never writes OpenTraces bucket internals. | M |
| OTR-803 | DONE | Implement the external `opentraces-agentd-adapter` executable against AgentD observer protocol v1. | Twenty-three adapter tests cover hello/health/event/ack/flush/shutdown, private append/fsync, canonical `sha256:` raw-hash validation, gap/conflict detection, stable UUID linkage, concurrent duplicate delivery, partial-write recovery, degraded retry, projectless local ingestion, private derived-file creation, and clean restart. | L |
| OTR-804 | DONE | Normalize AgentD Claude/Codex events into OpenTraces trace evidence through the selected external capture seam. | Parser tests reconstruct initial and two-phase follow-up prompts, provider content, Claude/Codex tool shapes/results, usage, errors, lifecycle/control evidence, distinct terminal states, provider/job/generation/image/sandbox linkage, and exact raw hashes. | L |
| OTR-805 | DONE | Qualify AgentD → adapter → local OpenTraces end to end. | `TestOpenTracesAdapterQualification` launches the real globally installed adapter through AgentD's stripped process environment, ingests content/tool/terminal events, auto-flushes a queryable trace, restarts the adapter, and proves duplicate replay retains the same UUID. A real projectless ephemeral canary retained 104 exact events under `0600`, linked trace `e68d4b55-732c-43dd-a15f-9b9d7153d4d6`, and produced one queryable local trace across two AgentD restarts with no duplicate rows. Phase-7 manager restart/upgrade tests retain AgentD-ledger/checkpoint coverage; remote status remains unconfigured. | L |
| OTR-806 | DONE | Package/install the adapter globally while leaving AgentD/Trading Floor adoption explicitly opt-in. | Editable pipx exposes `/Users/danieliser/.local/bin/opentraces-agentd-adapter`; the user subsequently opted AgentD in through private `~/.agentd/plugins.json`, while Trading Floor adoption remains its own decision. | S |

**Gate G5 — APPROVED 2026-08-09:** user authorized global OpenTraces setup and the actual external AgentD adapter, while leaving Trading Floor's direct/internal adoption to that product's own decision.

### Phase 9 — Release and local activation

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| REL-901 | DONE | Qualify and publish the breaking durable-native release as `v2.0.0`. | Local race, vet, build, real-Docker, real-OpenTraces, cross-build, and wheel checks pass; GitHub CI passes on Go 1.24.2 and 1.26.x; annotated tag, GitHub release, and four platform wheels are published. | M |
| REL-902 | DONE | Activate local-only OpenTraces and run the published AgentD against the persistent user data root. | Global `agentd --version` reports `2.0.0`; the live capability handshake reports OpenTraces `0.4.13` healthy with zero unacknowledged events; AgentD uses `~/.agentd`; remote sync remains unconfigured. | S |

### Phase 10 — Trading Floor admission and execution-policy qualification

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| ACT-1001 | DONE | Replace divergent HTTP, internal, resume, and reconstruction command assembly with one canonical request-to-runtime resolver. | HTTP create, internal chat spawn, and lost-generation resume use `resolveNativeExecution`; tests prove model/effort/fast/Claude turn-tool controls survive each path and the durable reconstruction manifest. Codex app-server receives strict CLI model/effort/tier overrides, materialization preserves the same values without mutating caller config, and the full race suite passes. | L |
| ACT-1002 | DONE | Add a loopback-safe listener and authenticated v1 API boundary. | `--host` defaults to literal `127.0.0.1`; an atomically published stable token lives below the `0700` data root with `0600` mode; every private HTTP/WebSocket/chat route uses constant-time bearer admission; browser WebSockets use same-origin checks and a credential-bearing subprotocol without URL leakage; dashboard, CLI, TUI, and Go clients authenticate; server/body/header limits and capability evidence pass under the full race suite. | L |
| ACT-1003 | DONE | Define and resolve a versioned caller execution policy without silent widening. | Policy `1.0` canonicalizes the proven Docker public-research grant, records its SHA-256 in the immutable existing manifest, and exposes policy/hash on create/inspect without a migration. Local execution, host mounts, MCP, hooks/teams, ambient/provider config, unknown tools, persistence, broader network/workspace, and interactive approval fail with structured `execution_policy_unsupported` before session creation or spawn. | L |
| ACT-1004 | DONE | Enforce the resolved policy consistently in Claude and Codex native transports. | Claude restricted sessions use `dontAsk`, exact `--tools`, safe mode, empty strict MCP config, and no permission bypass. Codex app-server disables non-granted execution/plugin/MCP features; thread bootstrap and every prompt retain the admitted sandbox/approval policy without `dangerFullAccess`. Docker restricted sessions drop every capability, use a read-only root and explicit writable tmpfs only when granted, enforce CPU/memory/PID/FD limits, and label `*-policy-v1`; strict resume JSON plus stored-manifest revalidation prevents widening. Provider flag probes, contract tests, vet, and the full race suite pass. | L |
| ACT-1005 | DONE | Add a bounded caller-provided JSON Schema output contract and terminal result proof. | Canonical JSON Schema and its hash are immutable manifest fields; Claude CLI and Codex `turn/start` receive the exact native schema. AgentD independently enforces a 1 MiB default/4 MiB ceiling, commits exact validated bytes as one replayable `output.final` event, exposes authenticated `/result`, and links its SHA-256 to the terminal receipt. Unsupported admission plus invalid and oversized terminal output use typed visible failures; focused, full, vet, and race suites pass. | L |
| ACT-1006 | DONE | Add an AgentD-created empty ephemeral Docker workspace with explicit retention and read-only support. | Policy v1 canonicalizes `workspace_retention=terminal_receipt`, rejects private provider context/credential paths, disables ambient credential discovery, creates private generated provider homes, and removes their state plus the container only after immutable receipt commit with startup retry. Restricted agents use a separate internal-only Docker network and a dual-homed allowlisting proxy, with no host bypass. A real non-root/read-only Docker probe proves an empty workspace, no host project/credentials/AutoMem/MCP, blocked direct egress, working proxied HTTPS, and terminal cleanup. | L |
| ACT-1007 | DONE | Expand capability and package identity proof. | The authenticated capability handshake and typed client report exact version/commit, API/event/plugin/policy versions, listener/auth transports, native structured-output limits/result endpoint, replay/runtime support, and the no-ambient-data ephemeral workspace profile. Canonical `pkg/buildinfo` identity powers `--build-info` and exact `--require-build VERSION@COMMIT`; release builds inject and verify both values, a deliberately mismatched artifact is rejected, and full tests/vet/race pass. | M |
| ACT-1008 | DONE | Run the real Trading Floor public Source Scout acceptance canary. | A Trading Floor-shaped authenticated request on literal loopback completed through one paid Codex Docker generation with the exact requested model/effort, empty read-only workspace, native public-search-only grant, and a schema-valid receipt-linked result. Duplicate create retained one session/generation; two daemon restarts returned byte-identical 104-event replay; OpenTraces retained one stable 104-event trace with no duplicate effect; live cancel was idempotent and distinct; Trading Floor's unavailable-AgentD health test retained healthy deterministic workers. The separate Trading Floor adapter update remains product-owned and is not bypassed by this runtime qualification. | L |
| ACT-1009 | DONE | Publish and activate the qualified `v2.1.0` installed artifact. | Tag/release CI built and published six version-and-commit-stamped wheels; the pipx-installed binary passes `--require-build 2.1.0@8e28b397f71c87b33aad6c404490f64d30d8bbeb`. A private-umask launchd service runs Docker-default AgentD on `127.0.0.1:38093` against `~/.agentd`; its authenticated live handshake reports loopback, API v1, event/policy/plugin schema 1.0, durable sequence replay, structured results, ephemeral workspace isolation, and OpenTraces 0.4.13 healthy with zero lag. | M |
| ACT-1010 | DONE | Eliminate generationless durable-admission orphans. | A bounded, read-only Docker CLI/daemon check now precedes new durable admission without blocking idempotent lookup. Any failure after admission returns the admitted identity as terminal with a synthetic unstarted generation, immutable terminal event, and receipt. Confirmed startup recovery terminalizes created/starting generationless admissions as `indeterminate`; cancel/terminate can close them idempotently without conflating their reasons. Build, vet, full race, real Docker isolation/reconstruction, and OpenTraces qualification pass. | L |
| ACT-1011 | IN PROGRESS | Qualify the installed daemon environment, not only the repository template. | The canonical launchd installation and installed-artifact qualification prove the effective daemon PATH resolves Docker from `/usr/local/bin` or `/opt/homebrew/bin`, while preserving loopback/auth/private-umask constraints. | S |
| ACT-1012 | IN PROGRESS | Keep OpenTraces catch-up from blocking AgentD startup. | Startup trace replay must be bounded so a large best-effort backlog degrades plugin health while the authenticated runtime API becomes available. Background replay remains checkpointed, idempotent, and restart-safe. | S |

**Gate G6 — APPROVED 2026-08-10:** the user supplied the Trading Floor activation audit and authorized implementation. The locked boundary is authenticated loopback admission plus an enforceable, inspectable, non-widening execution policy. Existing durable/replay/OpenTraces ownership does not change.

## 7. Qualification tests

These are release blockers, not optional follow-ups.

- [x] `Q-01` Concurrent duplicate create with the same key/hash returns one logical session and starts one provider process.
- [x] `Q-02` Same key with a different request hash returns structured `idempotency_conflict`.
- [x] `Q-03` Restart AgentD during an active Claude Docker session; reconnect from pointer with no new session.
- [x] `Q-04` Restart AgentD during an active Codex Docker session; reconnect from pointer with no new session.
- [x] `Q-05` Disconnect a chat client during content/tool streaming; replay from last contiguous sequence and continue live.
- [x] `Q-06` Intentionally remove/corrupt a stored event; replay reports a gap and never silently advances.
- [x] `Q-07` Restart after terminal completion; inspect returns the identical immutable receipt and last sequence.
- [x] `Q-08` Resume an eligible nonterminal session; runtime generation increments while session ID and event sequence continue.
- [x] `Q-09` Repeat reconnect/resume/control requests; no duplicate provider process, event identity, or side effect.
- [x] `Q-10` Cancel during tool execution; terminal reason is `cancelled`, distinct from interrupt, timeout, crash, and failure.
- [x] `Q-11` Stop admission and drain with queued requests and active Docker sessions; no new runtime starts after the gate closes.
- [x] `Q-12` Kill AgentD after event persistence but before publish; the same event is replayed with its original ID/sequence.
- [x] `Q-13` Kill AgentD after provider output but before durable persistence; reconstruct exactly or mark `indeterminate`.
- [x] `Q-14` Verify provider raw JSON is byte-equivalent after store/replay and derived views never mutate it.
- [x] `Q-15` Verify stdout/stderr/control/lifecycle/terminal channels remain distinguishable after restart.
- [x] `Q-16` Run a long stream larger than all in-memory buffers; durable replay remains complete from sequence 0.
- [x] `Q-17` Prove no old sidecar byte offset can be confused with a v1 sequence cursor.
- [x] `Q-18` Expire a native session before and after AgentD restart; both retain requested/dispatched timeout proof and finish `timed_out` without resetting the generation deadline.
- [x] `Q-19` Start with the OpenTraces adapter absent, slow, or crashing; `best_effort` execution continues while health and unacknowledged lag are explicit.
- [x] `Q-20` Restart AgentD and the adapter independently; delivery resumes after the last acknowledged sequence and replay produces no duplicate trace effects.
- [x] `Q-21` Reject incompatible plugin API/event-schema handshakes before required-policy work is admitted.
- [x] `Q-22` Send a forged/mismatched acknowledgement or corrupt/future checkpoint; AgentD refuses advancement and replays safely or reports the ledger gap.
- [x] `Q-23` Prove a plugin cannot invoke lifecycle controls, inherit ambient credentials, dynamically install itself, or trigger OpenTraces remote synchronization.
- [x] `Q-24` Upgrade/replace the external OpenTraces adapter while AgentD retains its event ledger and resumes delivery from a compatible checkpoint.
- [x] `Q-25` Every create/resume/reconstruction path resolves and applies identical model, effort, timeout, provider, and tool controls.
- [x] `Q-26` Default listener is literal loopback; every private v1 HTTP/WebSocket route rejects missing, malformed, and incorrect authentication without leaking the token.
- [x] `Q-27` Restart preserves private token-file identity and authenticated clients reconnect without putting credentials in URLs, argv, logs, events, manifests, or traces.
- [x] `Q-28` Unsupported or widened execution/tool policy fails before a provider process starts; resume cannot add tools, mounts, MCP servers, network, or permissions.
- [x] `Q-29` Restricted Claude and Codex sessions expose only the resolved tool grant and do not use unrestricted host/provider permission modes.
- [x] `Q-30` Caller JSON Schema produces exact bounded schema-valid bytes and an immutable receipt-linked result hash; unsupported, invalid, and oversized output fail visibly.
- [x] `Q-31` Empty ephemeral workspace cannot read the host project, credentials, AutoMem, ambient MCP configuration, or undeclared secret grants.
- [x] `Q-32` Installed package identity and capabilities prove version, commit, listener, auth, policy, structured-output, workspace, replay, runtime, and plugin compatibility.
- [x] `Q-33` Trading Floor public Source Scout acceptance canary passes end to end without granting product authority to AgentD or OpenTraces.
- [x] `Q-34` Stopping AgentD leaves Trading Floor deterministic collectors healthy and produces no hidden fallback execution.
- [x] `Q-35` With Docker absent before admission, create fails without writing a durable session.
- [x] `Q-36` If runtime spawn fails after admission, create returns the same session identity in a terminal failed/indeterminate state with replayable terminal evidence and a receipt.
- [x] `Q-37` Restart with a durable created/starting session and no generation terminalizes it; cancel/terminate can close the same state idempotently.
- [ ] `Q-38` The installed launchd service's effective PATH resolves Docker and a real Docker create succeeds after service restart.
- [ ] `Q-39` A slow or large OpenTraces catch-up cannot prevent the installed AgentD API from reaching healthy; checkpointed replay continues in the background.

## 8. Definition of done

This effort is done only when:

1. Claude and Codex native JSON are the canonical transport records.
2. AgentD commits each event before live publication.
3. Chat clients can reconnect after a sequence and receive stored-then-live events without an unreported gap.
4. Duplicate create returns the same logical session instead of `409`, except request-hash collision.
5. Running Docker sessions are reconstructable after AgentD restart without starting a second provider process.
6. Terminal sessions and receipts survive restart and cannot return to running.
7. All Q-series tests pass, including real-Docker restart tests with preserved artifacts.
8. The old sidecar semantic protocol and network endpoint are removed.
9. Full `go test ./...` is green and durable/restart test evidence is retained.
10. External observers consume immutable events through a versioned, replay-safe protocol without gaining execution authority.
11. The OpenTraces adapter can be absent or independently upgraded without AgentD owning OpenTraces data/schema lifecycle.
12. Every public admission path applies one canonical resolved runtime configuration and proves it in the durable manifest.
13. Sensitive v1 routes are authenticated and the daemon binds literal loopback by default.
14. Caller execution/tool/output/workspace policy is fail-closed, immutable, inspectable, and cannot widen on resume.
15. The real Trading Floor public canary and installed-artifact qualification pass with retained evidence.

## 9. Execution order

```text
G0 transport proof
  → G1 durable schema/contracts
    → native ingestion + durable append
      → replay/live subscription
        → idempotent resume
          → Docker reconstruction
            → compatibility + sidecar retirement
              → G3 qualification
                → G4 external observer protocol
                  → OpenTraces adapter qualification
```

Do not parallelize work across a gate whose contract has not been approved.

## 10. Scope change log

- 2026-08-09 — Initial sheet created from the AgentD/Trading Floor gap review.
- 2026-08-09 — User clarified priorities: native Claude/Codex JSON replaces sidecar semantics; chat output must be buffered/stored/replayable by pointer; duplicate create must resume/lookup rather than `409`; proper streaming supplies the event protocol; Docker sessions must be durable/reconstructable.
- 2026-08-09 — User approved removing non-native/sidecar execution methods after Claude and Codex native parity; retained lifecycle surface is start, fire-and-forget, attach/reconnect, follow-up/steer, generation resume, interrupt/cancel/terminate, inspect, receipt, and history.
- 2026-08-09 — User approved migration v2 for one-way provider identity discovery: an empty generation provider ID may bind once, while a known ID remains immutable.
- 2026-08-09 — User expanded scope to OpenTraces, then clarified that the traces system must remain externally maintained. AgentD will provide a generic immutable-event observer protocol, replay/checkpoints, supervision, health, and linkage; it will not own OpenTraces schemas, buckets, upgrades, or remote synchronization.
- 2026-08-09 — User authorized the external OpenTraces adapter and global machine setup. Global personal Claude/Codex/git capture is in scope; Hugging Face authentication/remote sync and automatic AgentD/Trading Floor enablement remain opt-in and are not implied.
- 2026-08-10 — User explicitly activated the OpenTraces observer for AgentD, retained local/privately controlled storage only, and left direct/internal Trading Floor use to that product.
- 2026-08-10 — User authorized a release after qualification; the breaking provider-native and durable-session contract is released as `v2.0.0`.
- 2026-08-10 — Trading Floor returned an activation audit identifying dropped native model controls, all-interface unauthenticated HTTP, unproven/widened provider permissions, missing unified tool restrictions, structured result proof, and empty ephemeral workspace. The user authorized implementation; these requirements are locked as Phase 10 and Gate G6 without broadening OpenTraces authority.
- 2026-08-10 — ACT-1007 completed. AgentD now exposes exact source identity and all admission-critical contract versions/profiles through capabilities and the Go client; release packaging injects both version and commit, and `--require-build` rejects checkout/installed-artifact drift before paid work is submitted. ACT-1008 acceptance canary is now active.
- 2026-08-10 — ACT-1008 first live probe failed closed when the explicitly supplied API key received upstream `401 Unauthorized`; AgentD retained all 33 ordered error/terminal events, committed a failed receipt, and removed the ephemeral container/state. Added the advertised `AGENTD_CODEX_AUTH_JSON` secret grant: it validates a caller-supplied object, materializes it as a private one-session `auth.json`, removes it from the Docker environment and durable manifest, retains only the grant name, and never restores ambient credential discovery.
- 2026-08-10 — The authenticated live Codex probe proved model `gpt-5.6-terra`, effort `high`, approval `never`, read-only sandbox, and native `webSearch`, then failed visibly because schema-shaped commentary was concatenated with the valid final answer. A red regression now binds Codex structured output only to the completed provider `final_answer` item; commentary deltas remain replayable evidence but cannot enter the admitted artifact.
- 2026-08-10 — External OpenTraces qualification exposed a real wire mismatch hidden by its fixture: AgentD emits canonical `sha256:<hex>` raw hashes while the adapter expected bare hex. OpenTraces commits `7cfdaa87` and `8b5210ee` now validate the canonical form, provide an explicit local project fallback for projectless ephemeral sessions, and force private process creation modes. The adapter suite passes 23 tests and remote synchronization remains unconfigured.
- 2026-08-10 — The authenticated public Source Scout canary completed in one Docker generation using `gpt-5.6-terra`/`high`, approval `never`, an empty read-only workspace, and only native public `webSearch`. Its schema-valid 123-byte result has receipt-linked hash `sha256:b9401dd330970e8427103e60174ba7785964abc4514d84deb82a78bd6c53b132`; all 104 events replay byte-identically after repeated AgentD restarts. Repeating the same create returned HTTP 200 with the identical terminal session and left exactly one logical session/generation. A second live session was cancelled after web-search tool activity; first cancel returned 202, the identical retry returned 200/idempotent, and the immutable receipt is `cancelled` at sequence 80. OpenTraces is healthy with zero lag and stable, duplicate-free trace linkage. At this checkpoint ACT-1008 still awaited Trading Floor collector-independence evidence.
- 2026-08-10 — ACT-1008/Q-33/Q-34 completed. Trading Floor's `tests/service/test_daemon_health.py` passes all five cases and proves unavailable AgentD cannot make deterministic workers unhealthy or trigger a direct-CLI fallback. Full AgentD tests, vet, race, real restricted-Docker isolation, real Claude/Codex whole-daemon restart, and real external OpenTraces qualification pass. The Docker restart harness itself was corrected to inspect the container namespace explicitly so the intentionally retained `agentruntime-proxy:latest` image can no longer cause a false skip. ACT-1009 now owns `v2.1.0` publication and installed-artifact activation.
- 2026-08-10 — The first ACT-1009 CI run exposed a fast-exit Docker log race on Go 1.26: `exec.Cmd.Wait` could close `StdoutPipe` before a delayed consumer drained the retained provider record. A deterministic red test now waits before reading; native Docker log stdout/stderr use parent-owned OS pipe readers whose EOF follows the child descriptors rather than `Wait`, and the focused race test passes 200 consecutive runs. Real-Docker diagnostics now drain both streams and retain stderr on failure.
- 2026-08-10 — Repeated cold-start isolation qualification then exposed admission racing Squid startup: the proxy container could be running and network-connected before port 3128 accepted traffic, causing a valid restricted session to fail with curl exit 7. AgentD now probes the proxy's internal listener with a bounded, portable `nc`/Bash fallback before admitting the container, including existing/raced proxy paths. The deterministic test requires a failed first probe and successful retry; three consecutive cold real-Docker policy qualifications and the minimal-Alpine whole-daemon reconstruction fixture pass.
- 2026-08-10 — ACT-1009 completed. Hosted CI passes on Go 1.24.2 and 1.26.x; annotated `v2.1.0`, six PyPI wheels, and the GitHub release point to `8e28b397f71c87b33aad6c404490f64d30d8bbeb`. The published pipx artifact is now supervised by launchd with Docker as default runtime, loopback-only port 38093, `~/.agentd` durability, a `077` umask, and healthy zero-lag local OpenTraces. Trading Floor's retained adapter still uses the superseded `caller_policy*`/`result_sha256` dialect; AgentD does not advertise those aliases because accepting only their capability name would falsely claim policy/result compatibility. That product must send `execution_policy` 1.0, bearer authentication, structured output, and consume `/result` plus receipt `artifact_hash` before activation.
- 2026-08-10 — Trading Floor's first real v2.1 canary exposed two release-qualification gaps. The manually activated launchd plist omitted the installer template's Docker-capable PATH, so recovery could not execute Docker until Trading Floor corrected the live plist. A create attempted while Docker was unavailable durably reached `starting`, returned HTTP 500 without its identity, and restart/cancel left it generationless. ACT-1010/1011 and Q-35–38 are release-blocking; the active AMAT canary must not be interrupted while the hotfix is developed.
- 2026-08-10 — ACT-1010 and Q-35–37 qualified. New Docker admissions receive a bounded read-only availability proof; post-admission launch failures return a terminal identity with replayable evidence; both `created` and `starting` generationless admissions recover to inspectable `indeterminate` receipts; and explicit cancel/terminate closes the same orphan state idempotently. Build, vet, full race, real Docker restricted-workspace and Claude/Codex restart reconstruction, plus the local OpenTraces adapter qualification all pass. The live canary is terminal, so ACT-1011 installed-service qualification may proceed without interrupting active work.
- 2026-08-10 — Installed `v2.1.1` qualification exposed an independent OpenTraces startup issue: the adapter correctly resumed a roughly 4,500-event checkpoint backlog, but AgentD synchronously waited for the entire best-effort catch-up before opening HTTP. ACT-1012/Q-39 are release-blocking for activation; the stuck supervised process was stopped while a bounded-startup fix is qualified.

## 11. Progress log

Append dated entries; do not rewrite history.

- 2026-08-09 — Planning sheet created. No implementation authorized or performed.
- 2026-08-09 — User authorized execution. G0 fixtures, real-Docker attach/recovery qualification, and ADR-001 completed. Direct native Docker transport is proposed; production transport awaits Gate G0 approval.
- 2026-08-09 — User approved G0 and direct removal of sidecar semantics. Durable typed contracts, structured errors, race-tested in-memory reference store, and the no-migration G1 schema proposal were added.
- 2026-08-09 — User approved G1. Pinned pure-Go SQLite, added the approved migration/store, verified concurrent idempotency and contiguous append, restart-stable active/terminal state, deliberate gap/raw corruption detection, and consistent hashed backup/restore.
- 2026-08-09 — User selected `~/.agentd` as the default persistence root. Database, backups, chat JSON/history, logs, credentials, and reconstructable runtime files share that root; `--data-dir` and `AGENTRUNTIME_DATA_DIR` still relocate it as one unit.
- 2026-08-09 — Added the shared Claude/Codex native transport, exact-record event broker, commit-before-publish fanout, sequence replay API, race-free stored-to-live WebSocket handshake, explicit slow-subscriber recovery, and discoverable replay boundaries.
- 2026-08-09 — Added reviewed migration v2 and typed one-way provider identity binding, including v1 database upgrade coverage and immutable second-bind rejection.
- 2026-08-09 — Added versioned durable create/inspect: concurrent/restarted duplicate requests return the same session, one admitted request spawns one generation, changed request hashes return `idempotency_conflict`, terminal receipt state survives reopen, and Docker receives durable reconciliation labels.
- 2026-08-09 — Durable request manifests now exclude explicit environment and nested request secrets, record grant references without values, and reject obvious undeclared secret environment keys before admission.
- 2026-08-09 — Durable Claude/Codex Docker generations now bypass the execution sidecar: native stdin uses `docker attach`, canonical output uses retained `docker logs --follow`, and exit status uses `docker wait`. Full provider arguments are preserved, Codex app-server bootstrap is correlated, provider identity binds durably, and a committed terminal event precedes the immutable receipt.
- 2026-08-09 — Fixed local native pipe ownership after race testing proved `exec.Cmd.Wait` could close fast provider output before ledger ingestion; provider drains are now independent from process wait ordering.
- 2026-08-09 — Startup reconciliation now reattaches the exact running generation without replaying provider initialization, deduplicates the retained source prefix to its original event identities/timestamps, marks confirmed missing generations `lost`, and commits `indeterminate` terminal proof for duplicate container claims.
- 2026-08-09 — Added explicit generation N+1 resume for lost nonterminal sessions, immutable terminal-receipt retrieval, bidirectional Claude stream-json launch, active native prompt/steer input, and native interrupt. Resume secret values are launch-only and must be regranted; nested secret paths conservatively require a new create request.
- 2026-08-09 — Added durable two-phase control idempotency without a schema migration. Prompt, steer, and interrupt keys commit `control.*.requested` before provider I/O and `control.*.dispatched` after the write; repeated completed calls are no-ops, changed key reuse conflicts, and a requested-only retry is explicitly `indeterminate`.
- 2026-08-09 — Completed native cancel semantics: cancel intent is committed before process termination, the watcher waits for durable dispatch proof before emitting `session.cancelled`, the immutable receipt records `cancelled`, and identical retries remain successful after termination. Repeated and race-enabled lifecycle tests pass; Gate G2 is approved and sidecar retirement may begin.
- 2026-08-09 — Removed the execution sidecar binary and runtime path. `local` now means direct provider process stdio; Docker always launches the requested command directly with attach/logs/wait and never publishes or probes port 9090. The container image no longer builds or contains a sidecar, install scripts build one daemon, and non-native Grok/Cursor agents are no longer admitted by the default registry. The unversioned daemon bridge remains temporarily as a client compatibility surface.
- 2026-08-09 — Added controlled shutdown admission gating and bounded drain. Once shutdown begins, create, resume, and chat-backed spawn cannot enter runtime admission. Non-reconstructable local work is stopped after the deadline; active Docker generations are neither killed nor stripped of the network/proxy infrastructure required for startup reattachment.
- 2026-08-09 — Docker durable admission now inspects the launched container's immutable image ID, rejects missing/malformed digest proof, exposes it through the runtime handle, and persists it in both initial and resumed runtime-generation records. The direct attach transport proof also passed against Docker 29.4.0 with `alpine:3.20`.
- 2026-08-09 — Completed Docker generation identity labeling. Before launch, AgentD resolves the selected image reference to a `sha256:` ID and adds image reference, digest, and sandbox-profile labels beside the existing logical-session/job/hash/generation labels. After launch it verifies the container's actual image ID matches admission and aborts on a tag race or malformed proof.
- 2026-08-09 — Closed the Docker-start/generation-commit crash window. Startup can idempotently reconstruct initial generation 1 or explicit replacement N+1 only when job key, request hash, agent, image reference/digest, sandbox profile, and container identity match the durable admission; unverifiable candidates are killed and receive an immutable `indeterminate` receipt. Codex can recover a not-yet-bound thread ID from retained `thread/started` output without issuing duplicate initialization, including when retained output wins the bootstrap race.
- 2026-08-09 — Docker terminal recovery now requires `docker wait` to agree with inspected container state. OOM-kill proof, runtime error detail, and observed start/finish timestamps propagate through native transport; proven OOM exits become `crashed` receipts with `SIGKILL` rather than generic exit-137 failures. Arbitrary 128+N codes are deliberately not claimed as signal proof.
- 2026-08-09 — Began compatibility-client migration without mixing cursor domains. `agentd attach --since` now means a durable event sequence, reads `/api/v1/ws/sessions/{id}/events`, renders typed payloads, exits on the durable terminal event, and sends prompt/steer/interrupt through caller-idempotent v1 control endpoints. `--no-replay` snapshots the durable tail before subscribing.
- 2026-08-09 — Moved production named-chat execution onto durable native sessions. Internal Claude/Codex chat spawns now admit a v1 logical session/generation and attach the canonical event broker; running-chat follow-ups use a stable chat-derived idempotency key and the same requested/dispatched control ledger instead of raw provider stdin. The old raw-handle injection remains only as a compatibility test seam pending route deletion.
- 2026-08-09 — Replaced sidecar-era public documentation. README, architecture, and contributor notes now document direct provider-native transport, v1 event sequences, durable controls/receipts, Docker reconciliation, the unified `~/.agentd` layout, and the remaining unversioned client-migration boundary without presenting byte cursors as durable proof.
- 2026-08-09 — Added typed v1 Go-client support for idempotent durable dispatch, logical-session inspect, sequence replay with exact raw decoding, and terminal-aware raw event streaming. One-shot dispatch derives a stable request key when omitted. `agentd dispatch` now uses this path end-to-end instead of polling unversioned byte logs.
- 2026-08-09 — Added the v1 compatibility handshake and typed Go client. Callers can reject incompatible AgentD/API/event schemas, providers, runtimes, replay persistence, Docker reconstruction, or plugin API support before submitting work; the empty plugin version list honestly reports that plugins remain deferred.
- 2026-08-09 — Enforced native runtime deadlines through the durable control and terminal ledger. Natural exit, cancel, and timeout now contend for one terminal boundary; timeout commits intent and dispatch proof before `session.timed_out` and its immutable receipt. Recovered generations use their original creation timestamp, so daemon restart cannot reset or extend the runtime limit.
- 2026-08-09 — Closed Docker terminal classification without guessing non-OOM signals. Positively observed signals and OOM remain `crashed`; AgentD-issued cancel and timeout retain their proven states; an otherwise unexplained Unix `128+N`-shaped exit is stored as `indeterminate` with an indeterminate generation and no fabricated signal value. DKR-504 is complete.
- 2026-08-09 — Added durable v1 session listing, including active and terminal history with timestamps, plus a typed Go-client method. Migrated the embedded dashboard from unversioned list/history/info/delete and byte-stream WebSocket routes to v1 list/inspect, sequence replay with gap checks, and idempotent cancel controls.
- 2026-08-09 — Migrated `agentd-tui` off the bidirectional compatibility WebSocket. Output uses stored-then-live v1 sequences with explicit gap checks; prompt, steer, and interrupt use idempotent HTTP controls. Chat history is rebuilt and delta-coalesced from the durable event ledger, and live attachment continues after the current session's loaded sequence.
- 2026-08-09 — Retired the orphaned standalone `cmd/dashboard`, a second 1,600-line UI whose spawn/stream/steer/kill/benchmark paths all used the compatibility API. The embedded v1 dashboard is now the single dashboard entry point. CMP-604 is complete.
- 2026-08-09 — Moved server chat history onto immutable v1 event ledgers while preserving the existing chain-index cursor shape. Pre-v1 NDJSON is retained only as read-only history explicitly labeled `legacy_ndjson_unverified`; durable projections are labeled `durable_v1`. Chat responses now return the active session's v1 event-stream URL and the old `/ws/chats/:name` byte proxy is removed. CMP-602 is complete.
- 2026-08-09 — Added reviewed additive migration v3 for immutable terminal reason proof. Existing receipts read their state as the default reason without rewriting; new administrative termination commits `control.terminate.*`, emits `session.terminated`, and persists reason `terminated`, distinct from caller cancellation while retaining the existing terminal lifecycle state.
- 2026-08-09 — Race qualification exposed unsynchronized native transport registration during immediate process exit and recovered attachment. Replaced closure-owned raw pointers with an atomic active-session reference across create, internal spawn, resume, and recovery; targeted and full API race suites now pass.
- 2026-08-09 — Completed the typed Go-client lifecycle surface before compatibility deletion: prompt/steer, interrupt, cancel, administrative terminate, lost-generation resume, and immutable terminal receipt. The capability handshake now enumerates every supported lifecycle control so callers can reject missing behavior before dispatch.
- 2026-08-09 — Removed the unused unversioned Go-client dispatch, session-info, kill, byte-log polling, and byte-stream methods plus their compatibility-only test matrix. The client package now has one canonical v1 session surface alongside the unversioned health probe.
- 2026-08-09 — Completed CMP-601/CMP-603. Removed all unversioned session, byte-log, and bidirectional daemon WebSocket routes; deleted `pkg/bridge`, compatibility schema responses, raw/steerable chat input, and unsafe reconstruction from unversioned NDJSON. Recovered processes without durable generation proof are stopped. A route-registration test prevents cursor-domain regression, and only v1 sequence replay remains externally addressable.
- 2026-08-09 — Reconciled qualification evidence for Q-01/02 and Q-05–18. Added an explicit WebSocket disconnect/reconnect test across an in-flight Codex tool call, strengthened cancellation qualification so a pending tool call is followed by `session.cancelled` without a fabricated tool result, and repeated the queued-admission shutdown race test under `-race`. Q-03/04 remain reserved for whole-daemon real-Docker restart qualification.
- 2026-08-09 — Completed Q-03/Q-04 against Docker 29.4.0 with real `alpine:3.20` containers running direct Claude stream-json and Codex app-server protocol fixtures. The harness kills the AgentD process group, restarts over the same private data root, proves the exact event-ID/sequence/raw-hash prefix is replayed, sends a second prompt over reattached stdin, and verifies generation 1 still owns exactly one container. Qualification exposed and fixed Docker `ps` ID truncation during recovery and missing constructor invariants on recovered sessions. All Q-series release blockers are complete.
- 2026-08-09 — Opened Phase 7 after Gate G4 approval. The locked boundary is an allowlisted external observer process over versioned NDJSON stdio, driven from AgentD's immutable durable ledger with acknowledgement checkpoints; OpenTraces remains independently maintained.
- 2026-08-09 — Completed Phase 7 and Q-19–24. AgentD now supervises explicitly allowlisted trace adapters with a clean environment, active compatibility/health handshake, nonblocking durable-ledger delivery, exact acknowledgement checkpoints, crash/upgrade-safe replay, scrubbed job/provider/sandbox context, caller-selectable admission policy, versioned health/link APIs, and typed SDK support. OpenTraces storage/schema/sync remain external. Full tests, vet, and targeted race suites pass.
- 2026-08-09 — Opened Phase 8 after Gate G5 approval. The global baseline is OpenTraces local-only global tracking with Claude/Codex observational hooks, git correlation, and shared skill; the external AgentD adapter will be packaged globally but not silently enabled for AgentD or Trading Floor.
- 2026-08-09 — Completed Phase 8. OpenTraces is globally installed from the independently maintained checkout through editable pipx, with `agentd`, Claude, Codex, and Pi capabilities. The AgentD adapter durably stores exact frames under a private local root, validates identities/hashes/sequences, recovers concurrent/partial/replayed delivery, normalizes runtime evidence through the registered OpenTraces ingest/security path, and auto-flushes terminal traces after acknowledgement. A real AgentD process-boundary qualification proves the trace is queryable and restart replay is duplicate-safe. Remote synchronization is unconfigured; AgentD and Trading Floor activation remain opt-in.
- 2026-08-10 — Activated the external OpenTraces adapter through private `~/.agentd/plugins.json`. A live AgentD 2.0 capability handshake reports the adapter healthy with zero ledger lag; capture remains local-only and Trading Floor is not coupled to OpenTraces.
- 2026-08-10 — Released `v2.0.0` from commit `8870212` after local and GitHub qualification. CI-only races in the malicious-prompt helper and fast Docker log draining were reproduced and fixed before the release tag. Six wheel build jobs and trusted PyPI publication passed; the GitHub release and four supported platform wheels are public. The globally installed daemon now reports `2.0.0` and runs against `~/.agentd`.
- 2026-08-10 — Opened Phase 10 after Gate G6 approval. Work begins with ACT-1001 because canonical runtime resolution is the dependency for honest policy hashing, provider enforcement, resume non-widening, and capability proof. Listener/auth follows before any real Trading Floor dispatch; structured output and ephemeral workspace remain release blockers rather than deferred claims.
- 2026-08-10 — Completed ACT-1001/Q-25. A single native resolver now supplies HTTP create, internal chat admission, and generation resume. It applies model, effort, fast tier, Claude turn/tool controls, and provider identity; Codex app-server receives strict command-line config overrides; and Codex materialization no longer hardcodes `high` or mutates the caller map. Stored manifests retain the same timeout/provider controls for reconstruction. Focused and full race suites pass.
- 2026-08-10 — Completed ACT-1002/Q-26/Q-27. AgentD now binds literal loopback by default and atomically owns one private bearer token under the unified data root. All v1 and chat mutation/inspection surfaces authenticate; browser streaming uses a same-origin authenticated subprotocol, and first-party Go/CLI/TUI clients load the token privately without URL/argv persistence. Missing, malformed, wrong, broad-mode, symlink, and concurrent-token cases are covered; `go vet ./...` and the full race suite pass.
- 2026-08-10 — Completed ACT-1003 without a schema migration. Explicit policy `1.0` is canonicalized and hashed into the existing immutable request manifest, returned by session views, and revalidated from that manifest on resume. The first supported profile is deliberately narrow: Docker, clean ephemeral workspace, read-only or ephemeral workspace-write filesystem, managed public HTTPS, approval never, no MCP/host mounts/hooks/teams/persistent volume, and only the canonical `web_search` grant. Unsupported or conflicting provider configuration fails before durable admission; the full test suite passes.
- 2026-08-10 — Completed ACT-1004/Q-28/Q-29. Restricted Claude receives an exact tool surface, safe mode, empty strict MCP config, and `dontAsk` without bypass flags. Restricted Codex disables shell/execution/plugin/MCP and other ungranted features at app-server launch; its admitted sandbox/approval policy is sticky across thread creation, resume, reconnect, and follow-up turns, replacing `dangerFullAccess`. Docker drops all capabilities, makes the root read-only, adds only policy-declared tmpfs writes, applies explicit resource limits, and records a policy-v1 sandbox profile. Resume accepts only prompt and regranted secrets, then revalidates the immutable stored policy. Installed provider flag probes, vet, and full race tests pass.
- 2026-08-10 — Completed ACT-1005/Q-30 without a database migration. Admission canonicalizes and validates caller JSON Schema, stores its hash in the immutable manifest, and passes the exact schema through Claude and Codex native controls. A bounded independent collector validates exact final bytes, commits them before terminal state as `output.final`, exposes authenticated retrieval, and links the raw hash to the immutable receipt; invalid/oversized output terminates with typed event evidence. Full tests, vet, and race qualification pass.
- 2026-08-10 — Completed ACT-1006/Q-31. Restricted materialization no longer imports cached or host Claude/Codex credentials and rejects caller private context/memory/credential paths; generated homes are private and contain empty MCP state. Policy agents sit only on an internal Docker network and reach allowlisted public HTTPS through a dual-homed proxy with no host bypass. The agent image now starts with an actually empty `/workspace`; read-only root/workspace and non-root execution are proven. Terminal-receipt retention removes containers and provider state with restart retry. The real Docker probe also exposed and fixed missing top-level-agent materialization and a Docker log-follower exit deadlock; full tests, vet, and race qualification pass.
