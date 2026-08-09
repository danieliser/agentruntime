# AgentD Durable Native Streaming — Task Sheet

Status: **G0 EVIDENCE COMPLETE / TRANSPORT APPROVAL PENDING**

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

### D6 — Retire semantics, not necessarily all in-container code

Target: remove the sidecar WebSocket normalization/replay/control protocol.

Gate G0 must decide between:

1. direct Docker create/start/attach with durable stdin reattachment; or
2. a minimal in-container transport owner that only preserves native stdin/stdout across AgentD reconnects.

If option 2 is required, it must not normalize events, assign public cursors, own replay policy, retry model work, or expose an unauthenticated host port. It is transport plumbing, not a second runtime protocol.

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

**Gate G0 — Transport decision:** user approves STR-004 before production transport code begins.

### Phase 1 — Durable contracts and store

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| DUR-101 | TODO | Define typed repository interfaces and structured domain errors for sessions, generations, events, and receipts. | Contract tests run against an in-memory fake and proposed SQLite implementation. | M |
| DUR-102 | TODO | Design SQLite schema, constraints, request hashing, transaction boundaries, backup/restore, and append-only protections. | Reviewed schema document; no migration yet. | M |
| DUR-103 | TODO | Implement idempotent logical-session creation and lookup transaction. | Concurrent duplicate starts create exactly one row/runtime admission. | M |
| DUR-104 | TODO | Implement atomic sequence allocation + event append. | Concurrent writers produce one contiguous sequence with stable event IDs. | L |
| DUR-105 | TODO | Implement immutable state transitions and terminal receipt persistence. | Restart returns the exact terminal receipt; terminal→running is rejected structurally. | M |

**Gate G1 — Durable schema/API review:** approve schema and contracts before adding migration files or public v1 routes.

### Phase 2 — Native Claude/Codex ingestion

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| NAT-201 | TODO | Add one canonical native transport interface for start, input, interrupt, output records, stderr, wait, and recovery metadata. | Claude and Codex adapters pass the same contract suite. | M |
| NAT-202 | TODO | Route Claude native stream-json input/output directly into AgentD ingestion. | Raw records round-trip byte-for-byte; content/tools/results remain streamable. | L |
| NAT-203 | TODO | Route Codex app-server JSON-RPC directly into AgentD ingestion. | Initialize/thread/turn/steer/interrupt/resume and notifications pass fixtures and live opt-in test. | L |
| NAT-204 | TODO | Add envelope derivation without raw-record loss. | Every stored native record has one stable envelope; derived types match fixtures. | M |
| NAT-205 | TODO | Separate provider stdout, runtime stderr, lifecycle, control acknowledgment, and terminal events. | No mixed writer/log can make stderr parse as provider JSON. | M |

**Gate G2 — Native-stream parity:** Claude/Codex streaming, control, resume, usage, and terminal parity must pass before old sidecar semantics are removed.

### Phase 3 — Buffered storage and cursor replay

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| REP-301 | TODO | Build commit-before-publish event fanout over the durable store. | Crash between append/publish replays the committed event once with the same identity. | L |
| REP-302 | TODO | Add paginated replay API using `after_sequence`. | Boundaries 0, middle, latest, future, terminal, and large histories are deterministic. | M |
| REP-303 | TODO | Add live stream handshake that closes the replay/live race. | Forced event during subscription appears exactly once by stable identity. | L |
| REP-304 | TODO | Detect sequence corruption/missing rows and return `event_gap`/`indeterminate`. | Intentionally removed event is detected; later events are not presented as contiguous. | M |
| REP-305 | TODO | Define retention/archive policy with no silent truncation. | Earliest available cursor is discoverable; archived sessions remain explicit. | M |

### Phase 4 — Idempotent reconnect and resume

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| RES-401 | TODO | Replace duplicate-create `409` with idempotent lookup semantics. | Same key/hash returns same session concurrently and after daemon restart. | M |
| RES-402 | TODO | Persist provider session/thread identity and resume inputs. | Claude/Codex continuation uses the exact stored provider identity. | M |
| RES-403 | TODO | Implement explicit runtime resume as generation `N+1` for eligible nonterminal sessions. | Logical ID and sequence continue; generation increments once. | L |
| RES-404 | TODO | Make interrupt/cancel/resume requests idempotent with durable outcomes. | Repeated controls have stable responses and no duplicate side effects. | M |

### Phase 5 — Docker reconstruction

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| DKR-501 | TODO | Persist and label session ID, job key, request hash, generation, container ID, image reference/digest, and sandbox-profile version. | DB and `docker inspect` can be reconciled without guessing identity. | M |
| DKR-502 | TODO | Implement startup reconciliation across expected/running/exited/missing/duplicate containers. | Each case has an explicit state transition or `indeterminate`; no implicit rerun. | L |
| DKR-503 | TODO | Reattach native input/output at the last durable boundary. | Restart during active output yields no missing or newly identified duplicate event. | L |
| DKR-504 | TODO | Recover terminal state when container exits while AgentD is down. | Exit reason/receipt is reconstructed or explicitly indeterminate. | M |
| DKR-505 | TODO | Add admission stop + bounded drain before daemon shutdown. | New starts are rejected during drain; active Docker generations remain recoverable. | M |

**Gate G3 — Docker durability qualification:** all restart/replay/resume tests pass against a real Docker daemon before declaring the session contract durable.

### Phase 6 — Compatibility and retirement

| ID | Status | Task | Acceptance evidence | Size |
|---|---|---|---|---|
| CMP-601 | TODO | Add a compatibility adapter for current clients during v1 rollout. | Existing chat/UI clients can migrate without consuming mixed old/new cursors. | M |
| CMP-602 | TODO | Decide treatment of legacy NDJSON logs: import, read-only history, or explicit legacy status. | No legacy file is silently treated as a complete v1 event ledger. | M |
| CMP-603 | TODO | Remove sidecar WS normalization/replay only after Gate G2; retain a minimal transport shim only if G0 requires it. | No second event identity/cursor authority remains. | M |
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
8. The old sidecar semantic protocol is removed or reduced to a G0-approved transparent transport owner.
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

## 11. Progress log

Append dated entries; do not rewrite history.

- 2026-08-09 — Planning sheet created. No implementation authorized or performed.
- 2026-08-09 — User authorized execution. G0 fixtures, real-Docker attach/recovery qualification, and ADR-001 completed. Direct native Docker transport is proposed; production transport awaits Gate G0 approval.
