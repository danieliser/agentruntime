# AgentD Durable Native Streaming — Task Sheet

Status: **G0 + G1 APPROVED / G2 IN PROGRESS**

Last updated: 2026-08-09

Primary scope: Claude + Codex JSON streaming, durable replay, idempotent resume, Docker reconstruction

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
| 10 | Plugins/OpenTraces | No execution-observer plugin API exists. | Explicitly deferred until the durable event store and replay contract are stable; plugins must consume that contract later. |
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

### Out of scope for this sheet

- DBOS workflows, retries, approvals, or artifact admission.
- OpenTraces and the general plugin framework.
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
| RES-404 | IN PROGRESS | Make interrupt/cancel/resume requests idempotent with durable outcomes. | Resume is serialized/idempotent and active native interrupt is exposed; durable control keys, cancellation classification, and crash-boundary outcomes remain. | M |
| RES-405 | DONE | Expose typed follow-up and steer input on an active native generation. | `POST /api/v1/sessions/{id}/input` routes prompt/steer through the registered Claude/Codex native transport; interactive Codex integration proves a second turn and interrupt without a sidecar. | M |

### Phase 5 — Docker reconstruction

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| DKR-501 | IN PROGRESS | Persist and label session ID, job key, request hash, generation, container ID, image reference/digest, and sandbox-profile version. | v1 admission persists session/generation and Docker durable labels/recovery metadata; resolved image digest and native sandbox profile remain. | M |
| DKR-502 | IN PROGRESS | Implement startup reconciliation across expected/running/exited/missing/duplicate containers. | Expected generations reattach, confirmed-missing generations become `lost`, duplicate claims become terminal `indeterminate`, and stopped labeled containers are discovered; crash-before-generation and full state inspection remain. | L |
| DKR-503 | IN PROGRESS | Reattach native input/output at the last durable boundary. | Fresh and recovered durable Docker handles use reattachable stdin plus retained logs directly and never query the sidecar port; startup ledger reconciliation remains. | L |
| DKR-504 | IN PROGRESS | Recover terminal state when container exits while AgentD is down. | Docker recovery now includes stopped containers and uses retained logs plus `docker wait`; signal/OOM/cancel reason qualification remains. | M |
| DKR-505 | TODO | Add admission stop + bounded drain before daemon shutdown. | New starts are rejected during drain; active Docker generations remain recoverable. | M |

**Gate G3 — Docker durability qualification:** all restart/replay/resume tests pass against a real Docker daemon before declaring the session contract durable.

### Phase 6 — Compatibility and retirement

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| CMP-601 | TODO | Add a compatibility adapter for current clients during v1 rollout. | Existing chat/UI clients can migrate without consuming mixed old/new cursors. | M |
| CMP-602 | TODO | Decide treatment of legacy NDJSON logs: import, read-only history, or explicit legacy status. | No legacy file is silently treated as a complete v1 event ledger. | M |
| CMP-603 | TODO | Remove sidecar WS normalization/replay after Gate G2. | No sidecar health port, command frames, replay buffer, byte cursor, or second event authority remains. | M |
| CMP-604 | TODO | Update docs, examples, SDK, and capability response for the v1 stream contract. | One canonical public entry point and versioned examples. | M |

## 7. Qualification tests

These are release blockers, not optional follow-ups.

- [ ] `Q-01` Concurrent duplicate create with the same key/hash returns one logical session and starts one provider process.
- [ ] `Q-02` Same key with a different request hash returns structured `idempotency_conflict`.
- [ ] `Q-03` Restart AgentD during an active Claude Docker session; reconnect from pointer with no new session.
- [ ] `Q-04` Restart AgentD during an active Codex Docker session; reconnect from pointer with no new session.
- [ ] `Q-05` Disconnect a chat client during content/tool streaming; replay from last contiguous sequence and continue live.
- [ ] `Q-06` Intentionally remove/corrupt a stored event; replay reports a gap and never silently advances.
- [ ] `Q-07` Restart after terminal completion; inspect returns the identical immutable receipt and last sequence.
- [ ] `Q-08` Resume an eligible nonterminal session; runtime generation increments while session ID and event sequence continue.
- [ ] `Q-09` Repeat reconnect/resume/control requests; no duplicate provider process, event identity, or side effect.
- [ ] `Q-10` Cancel during tool execution; terminal reason is `cancelled`, distinct from interrupt, timeout, crash, and failure.
- [ ] `Q-11` Stop admission and drain with queued requests and active Docker sessions; no new runtime starts after the gate closes.
- [ ] `Q-12` Kill AgentD after event persistence but before publish; the same event is replayed with its original ID/sequence.
- [ ] `Q-13` Kill AgentD after provider output but before durable persistence; reconstruct exactly or mark `indeterminate`.
- [ ] `Q-14` Verify provider raw JSON is byte-equivalent after store/replay and derived views never mutate it.
- [ ] `Q-15` Verify stdout/stderr/control/lifecycle/terminal channels remain distinguishable after restart.
- [ ] `Q-16` Run a long stream larger than all in-memory buffers; durable replay remains complete from sequence 0.
- [ ] `Q-17` Prove no old sidecar byte offset can be confused with a v1 sequence cursor.

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
```

Do not parallelize work across a gate whose contract has not been approved.

## 10. Scope change log

- 2026-08-09 — Initial sheet created from the AgentD/Trading Floor gap review.
- 2026-08-09 — User clarified priorities: native Claude/Codex JSON replaces sidecar semantics; chat output must be buffered/stored/replayable by pointer; duplicate create must resume/lookup rather than `409`; proper streaming supplies the event protocol; Docker sessions must be durable/reconstructable.
- 2026-08-09 — User approved removing non-native/sidecar execution methods after Claude and Codex native parity; retained lifecycle surface is start, fire-and-forget, attach/reconnect, follow-up/steer, generation resume, interrupt/cancel/terminate, inspect, receipt, and history.
- 2026-08-09 — User approved migration v2 for one-way provider identity discovery: an empty generation provider ID may bind once, while a known ID remains immutable.

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
