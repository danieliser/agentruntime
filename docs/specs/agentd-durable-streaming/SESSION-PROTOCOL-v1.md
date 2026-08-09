# AgentD durable session protocol v1

Status: native lifecycle and direct Docker launch implemented; restart qualification in progress

Task IDs: RES-401, RES-402, DKR-501

## Create or look up

```text
POST /api/v1/sessions
```

The request uses the existing flat session request with two v1 fields:

```json
{
  "idempotency_key": "trading-floor-job-123",
  "agent": "claude",
  "runtime": "docker",
  "prompt": "Perform the requested task",
  "env": {
    "NORMAL_SETTING": "persistable",
    "ANTHROPIC_API_KEY": "launch-only-value"
  },
  "secret_grants": ["ANTHROPIC_API_KEY"]
}
```

- first key/effective request: `201 Created` and exactly one runtime admission;
- same key/effective request: `200 OK` with the existing logical session;
- same key/different effective request: `409 idempotency_conflict`;
- a terminal match is returned as terminal and is never restarted; and
- an interrupted request after durable admission remains discoverable for
  reconciliation rather than silently spawning another paid session.

The effective request is canonicalized after runtime resolution. Explicit
environment secret values and secret-valued nested fields are removed before
persistence. Their stable grant names/paths are hashed and recorded, but their
values are neither hashed nor stored. Obvious sensitive environment keys that
are not declared in `secret_grants` are rejected before admission.

## Inspect

```text
GET /api/v1/sessions/{session_id}
```

Create/lookup and inspect return the same data shape:

```json
{
  "api_version": "v1",
  "data": {
    "session_id": "logical-session-id",
    "idempotency_key": "trading-floor-job-123",
    "agent": "claude",
    "runtime": "docker",
    "state": "running",
    "generation": 1,
    "last_sequence": 42,
    "events_url": "http://agentd/api/v1/sessions/logical-session-id/events",
    "event_stream_url": "ws://agentd/api/v1/ws/sessions/logical-session-id/events"
  }
}
```

Terminal proof is available separately:

```text
GET /api/v1/sessions/{session_id}/receipt
```

## Reconnect, input, and resume

Event reconnection remains cursor-only and never starts work. While a native
generation is active, callers may submit typed `prompt` or `steer` input and
interrupt the current turn:

```text
POST /api/v1/sessions/{session_id}/input
POST /api/v1/sessions/{session_id}/interrupt
POST /api/v1/sessions/{session_id}/cancel
```

Each mutating control requires an `idempotency_key`. AgentD records intent
before provider/process I/O and dispatch proof afterward. A completed retry is
a no-op success. A requested operation without dispatch proof is reported as
`indeterminate` rather than repeated. Cancel closes the active native process
and commits a distinct `session.cancelled` event and immutable `cancelled`
receipt; interrupt only affects the current turn.

A confirmed-missing generation is marked `lost`. It can be resumed under the
same logical session with a new prompt:

```text
POST /api/v1/sessions/{session_id}/resume
```

Resume creates generation N+1 exactly once and supplies the prior Claude
session ID or Codex thread ID. A running or terminal session is returned as a
`200` lookup/no-op. Environment secret values are never reconstructed from
disk; approved environment grants must be supplied again in the resume body.
Nested request-secret paths currently require a new create request.

## Runtime generation admission

The logical session is committed before runtime admission. A successful spawn
then records generation identity and transitions generation/session to
`running`. Docker launches receive labels for logical session ID, generation,
idempotency key, request hash, and task ID. Recovery reads those labels instead
of guessing identity from a container name.

Durable Claude/Codex generations do not publish or dial the execution-sidecar
port. Native input is connected with `docker attach`; exact provider output is
read from Docker's retained `json-file` log using `docker logs --follow`; and
the container result comes from `docker wait`. Containers and materialized
session files remain available after exit for reconciliation and receipt proof.

Resolved image digest capture, crash-before-generation reconciliation,
signal/OOM classification, controlled shutdown, and real-Docker restart
qualification remain required before Gate G3.
