# AgentRuntime Architecture

## Ownership boundary

AgentD is an isolated execution runtime. It owns logical session identity, concrete runtime generations, native provider transport, durable event ingestion/replay, lifecycle controls, Docker reconstruction, and immutable terminal receipts.

The caller owns workflow state, retries, approvals, scheduling, budgets, and final artifact admission. AgentD never independently retries an entire paid model session.

## Core data flow

```text
caller input
  → versioned AgentD control intent (durably requested)
  → Claude stream-json or Codex app-server JSON-RPC
  → exact provider/runtime record
  → SQLite append + sequence allocation
  → post-commit live publication
  → replay/live clients
```

Provider stdout, runtime stderr, lifecycle, control, and terminal records remain separate streams. Derived event types make querying convenient, but never replace or mutate the exact raw provider bytes.

## Durable model

SQLite under the data root contains four authorities:

- `sessions`: caller idempotency key, canonical request hash/manifest, agent/runtime, lifecycle state, active generation, last sequence;
- `runtime_generations`: runtime/container identity, image reference/digest, sandbox profile, provider session/thread ID, log configuration, generation state;
- `events`: immutable sequence, event ID, type/stream, payload, exact raw bytes/hash, timestamp;
- `terminal_receipts`: immutable final state, code/signal, timestamps, hashes, and final sequence.

Logical sequence numbers are contiguous across runtime generations. A terminal logical session never returns to running.

Provider identity is immutable after discovery. A generation may bind an initially empty Claude session ID or Codex thread ID once; later observations must match.

## Session state

```text
created → starting → running → completed
                    ↘ failed
                    ↘ cancelled
                    ↘ timed_out
                    ↘ crashed
                    ↘ indeterminate
```

Runtime generations move from `starting` to `running`, then `exited`, `lost`, or `indeterminate`. Explicit resume creates generation `N+1` under the same nonterminal logical session.

Reconnect, reconstruct, and resume are distinct:

- reconnect replays after a client sequence and continues live;
- reconstruct reattaches AgentD to the same Docker generation after daemon restart;
- resume creates a replacement generation only after the previous generation is durably lost.

## Native provider transport

`pkg/nativeprotocol` is the single provider-wire boundary.

Claude:

- `--input-format stream-json`
- `--output-format stream-json`
- prompt, steer, interrupt, approval, content, tools, usage, and terminal records stay native JSON

Codex:

- `codex app-server --listen stdio://`
- initialization and thread start/resume are correlated JSON-RPC calls
- retained `thread/started` output can restore a thread ID after a daemon crash without repeating initialization

The shared transport exposes start, bootstrap/reconnect, typed input, interrupt, exact stdout/stderr records, wait, close, and recovery metadata.

## Commit-before-publish replay

`pkg/eventstream` owns append and subscription.

1. Decode only enough to derive type/payload/provider identity.
2. Allocate event ID and next sequence transactionally.
3. Store the exact raw record and hash.
4. Publish the committed event to live subscribers.

A subscriber snapshots the durable tail, replays through that boundary, then consumes live events. Contiguity is checked at both stages. Slow subscribers are disconnected with an explicit recovery error and reconnect from their last contiguous sequence.

Future cursors, deleted rows, divergent raw hashes, and missing ranges fail explicitly; no cursor is silently advanced.

## Idempotent controls

Prompt, steer, interrupt, and cancel use two durable control events:

```text
control.<kind>.requested
  → one provider/runtime side effect
  → control.<kind>.dispatched
```

An identical completed retry is a no-op. Reusing a key for changed content conflicts. A requested-only retry is `indeterminate` because AgentD cannot prove whether the provider observed the side effect.

Cancellation commits intent before termination and waits for dispatch proof before emitting `session.cancelled` and the receipt.

## Docker runtime

The durable Docker workload is the provider process itself. There is no in-container execution broker or port 9090.

Fresh/recovered handles use:

- `docker attach --sig-proxy=false` for writable native stdin;
- `docker logs --follow --since=0` for retained plus live output;
- `docker wait` for exit code;
- `docker inspect` for labels, image identity, and terminal-state proof.

Before launch AgentD resolves the configured image reference to an immutable `sha256:` ID and labels the container with the logical admission and sandbox identity. After launch it verifies the container used the admitted digest.

On startup, DB and Docker are reconciled:

- exact expected generation: attach and reconcile retained records;
- stopped generation: ingest retained output and terminal state;
- confirmed missing generation: mark `lost`;
- duplicate generation claim: terminate candidates and finalize `indeterminate`;
- container started before generation commit: reconstruct only after exact job/hash/agent/image/profile/container validation;
- any unverifiable boundary: finalize `indeterminate` rather than guessing.

Controlled shutdown closes admission under a shared gate. Active Docker sessions are preserved for restart; non-reconstructable local work is bounded by the shutdown deadline.

## Local runtime

The local runtime launches the provider command directly and preserves output-drain ownership independently from `exec.Cmd.Wait`. It supports the native event and control contract while the daemon is alive. Durable restart reconstruction is intentionally a Docker-only qualification boundary.

## Named chats

Chat records are JSON under `<data-root>/chats` and hold configuration plus a logical session chain. Production Claude/Codex chat sessions enter through durable v1 admission, and follow-ups use the same idempotent native control ledger.

Compatibility NDJSON chat pagination remains during migration. SQLite session events are the durable execution authority.

## Module map

```text
cmd/agentd/              daemon and CLI commands
pkg/agent/               Claude/Codex command adapters
pkg/api/                 HTTP/WS v1 API, lifecycle, recovery, compatibility routes
pkg/chat/                named-chat JSON registry and orchestration
pkg/durable/             typed store contracts and state model
pkg/durable/sqlite/      SQLite authority, migrations, backup/integrity
pkg/eventstream/         immutable ingestion and replay/live subscriptions
pkg/nativeprotocol/      provider-native JSON adapters and transport
pkg/runtime/             local and direct Docker process handles
pkg/session/             live session manager and compatibility replay/log sink
docs/specs/              task sheet, ADRs, and qualification evidence
```

## Persistence root

Default: `~/.agentd` (or `AGENTRUNTIME_DATA_DIR`, or `--data-dir`).

Database, backups, chat JSON, logs, credentials, and reconstructable materialized files move together as one unit. SQLite is opened with private permissions and checked for integrity at daemon startup.

## Security posture

Docker command construction validates mounts, environment, paths, and runtime arguments. Durable secret grants record names without values. The runtime records image and sandbox identity for reconstruction.

The broader authenticated API and full sandbox-hardening epics are not complete. Until then, deploy AgentD only on a trusted interface or behind authenticated transport.

## Compatibility boundary

The old execution sidecar and redundant standalone dashboard are deleted. CLI attach/dispatch, the TUI, the embedded dashboard, and typed Go-client methods use v1. Remaining unversioned session/WS/log routes and byte cursors are migration-only surfaces for legacy Go-client methods and server chat history. They are a different cursor domain and will be removed after those consumers move to v1.
