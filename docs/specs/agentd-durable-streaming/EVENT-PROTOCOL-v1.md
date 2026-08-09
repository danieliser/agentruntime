# AgentD event protocol v1

Status: production native runtime, replay, terminal, and control ledger implemented

Task IDs: NAT-204, REP-301, REP-302, REP-303, REP-304, REP-305

## Authority and identity

Provider-native stdout records are the semantic authority. AgentD stores their
exact bytes before live publication and derives a query-friendly type/payload
without replacing those bytes. Runtime stderr is a distinct physical stream
and is never parsed as provider JSON.

Sequences are contiguous per logical AgentD session across runtime
generations. A client cursor is the highest contiguous sequence the client has
durably processed. Replayed records retain the same event ID, sequence,
timestamp, generation, raw bytes, and derived payload.

## HTTP replay

```text
GET /api/v1/sessions/{session_id}/events?after_sequence=N&limit=M
```

- `after_sequence` defaults to `0` and is exclusive.
- `limit` defaults to `100` and is capped at `1000`.
- a cursor beyond `last_sequence` returns `invalid_cursor`;
- a missing sequence returns `event_gap`; and
- storage uncertainty or raw-hash corruption returns `indeterminate`.

The response reports `earliest_sequence`, `last_sequence`, and `has_more`.
In v1, the complete event ledger is retained indefinitely: earliest is `1` for
a non-empty ledger and `0` for an empty ledger. A future archive implementation
must preserve event identities/sequences or advertise an explicit unavailable
range; it may never silently advance a stale cursor.

## Stored-to-live stream

```text
GET /api/v1/ws/sessions/{session_id}/events?after_sequence=N
```

The server subscribes before upgrading the socket, atomically snapshots the
durable tail, and first sends:

```json
{
  "frame_type": "stream.ready",
  "schema_version": "1.0",
  "session_id": "session-id",
  "after_sequence": 41,
  "earliest_sequence": 1,
  "replay_through": 47
}
```

It then sends committed event envelopes `42..47`, followed by live committed
events. This closes the replay/live race without changing cursor domains.

Each event frame contains:

```json
{
  "schema_version": "1.0",
  "event_id": "evt_stable-source-position-hash",
  "session_id": "session-id",
  "generation": 2,
  "sequence": 48,
  "timestamp": "2026-08-09T12:34:56.123456789Z",
  "type": "content.delta",
  "stream": "provider_stdout",
  "payload": {"text": "hello"},
  "raw_base64": "ZXhhY3QgcHJvdmlkZXIgYnl0ZXM=",
  "raw_sha256": "sha256:..."
}
```

`raw_base64` is used because decode/re-encode of a JSON object would not
preserve exact provider bytes. Consumers should use `payload` for normal UI
work and retain `(session_id, sequence, event_id)` for deduplication.

## Backpressure and reconnect

Live buffers are bounded. A slow subscriber never blocks provider ingestion or
durable commit. When its buffer is exhausted, AgentD emits a structured
`backpressure` stream error and closes that subscription. The client reconnects
using the last contiguous sequence it processed; the durable ledger supplies
the omitted portion.

## Outgoing control proof

Prompt, steer, interrupt, and cancel requests require a caller idempotency key. AgentD
commits a `control.{kind}.requested` event before writing to provider stdin and
a `control.{kind}.dispatched` event only after that side effect succeeds.

- A retry with both phases returns the prior success without another write.
- Reusing the key for different command content is an immutable conflict.
- A retry with intent but no dispatch proof returns `indeterminate`; AgentD
  never guesses whether a paid turn crossed the process boundary.
- Cancel termination does not become `session.cancelled` or an immutable
  `cancelled` receipt until `control.cancel.dispatched` is durable.

These events use `stream: control`, share the session sequence domain, and are
replayed like provider, stderr, lifecycle, and terminal events.

## Current trust boundary

These read-only routes inherit AgentD's existing daemon trust boundary. The
broader authenticated API rollout remains a separate task and is required
before exposing raw prompt/tool history beyond a trusted local or protected
deployment.
