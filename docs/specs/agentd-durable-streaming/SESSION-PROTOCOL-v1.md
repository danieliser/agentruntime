# AgentD durable session protocol v1

Status: idempotent admission and inspect implemented; native Docker resume in progress

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

## Runtime generation admission

The logical session is committed before runtime admission. A successful spawn
then records generation identity and transitions generation/session to
`running`. Docker launches receive labels for logical session ID, generation,
idempotency key, request hash, and task ID. Recovery reads those labels instead
of guessing identity from a container name.

The current Docker compatibility runtime still needs native transport wiring,
resolved image digest capture, startup DB↔Docker reconciliation, and stopped
container recovery before Gate G3.
