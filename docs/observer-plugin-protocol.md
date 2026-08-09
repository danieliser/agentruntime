# AgentD External Trace Observer Protocol v1

AgentD streams immutable execution evidence to separately installed trace
systems over NDJSON stdio. OpenTraces is the first intended adapter, but it is
not embedded in AgentD: OpenTraces owns its trace schema, capture parser,
bucket, migrations, upgrades, security review, and remote synchronization.

AgentD owns only:

- starting an explicitly allowlisted local adapter executable;
- protocol and event-schema compatibility checks;
- delivering committed events from the durable AgentD ledger;
- validating exact acknowledgements and persisting replay checkpoints;
- exposing adapter health, lag, and trace linkage;
- flushing and stopping the adapter during daemon shutdown.

The adapter has no AgentD API credentials or lifecycle messages. It cannot
start, prompt, steer, interrupt, cancel, resume, authorize, or admit work.

## Configure

Copy [`plugins.example.json`](../plugins.example.json) to the AgentD data root
and make it private:

```sh
cp plugins.example.json ~/.agentd/plugins.json
chmod 600 ~/.agentd/plugins.json
```

`--plugin-config` overrides the file. `AGENTRUNTIME_PLUGIN_CONFIG` is the
environment fallback. The command must be an absolute path; AgentD never uses
a shell. The child receives only the explicitly listed `environment` entries,
not AgentD's ambient environment or credentials.

`policy` is the default when a request selects this plugin without an explicit
policy. `best_effort` is the safe default. `required` rejects a first admission
unless the adapter is running, healthy, and compatible. It never gives the
adapter authority to stop an already admitted runtime.

A caller can override the configured default per session:

```json
{
  "idempotency_key": "trading-floor-job-42",
  "agent": "codex",
  "runtime": "docker",
  "prompt": "Inspect the repository",
  "trace": {
    "plugin": "opentraces",
    "policy": "required"
  }
}
```

An idempotent lookup of an existing job remains a lookup even if its adapter
later degrades. Trace health cannot cause AgentD to launch a replacement paid
session.

## Transport

The adapter is one long-lived subprocess. AgentD writes one JSON object per
line to stdin and reads one response per line from stdout. Stderr is not part
of the event protocol. Requests are serialized; the configured timeout bounds
every response. A timeout, malformed response, mismatched acknowledgement, or
process exit marks the adapter degraded/down without blocking provider event
ingestion.

### Handshake

AgentD sends:

```json
{"type":"hello","request_id":"uuid","plugin_api_version":"1.0","agentd_version":"build","event_schema_versions":["1.0"]}
```

The adapter returns:

```json
{
  "type":"hello",
  "request_id":"same-uuid",
  "plugin":{"name":"opentraces","version":"adapter-version"},
  "plugin_api_version":"1.0",
  "event_schema_versions":["1.0"],
  "capabilities":{
    "immutable_events":true,
    "idempotent_events":true,
    "trace_linkage":true
  }
}
```

AgentD rejects a different request ID, plugin name, API version, event schema,
or missing required capability.

### Event delivery

AgentD sends the complete committed event plus its scrubbed durable context:

```json
{
  "type":"event",
  "delivery_id":"same-as-event-id",
  "event":{
    "schema_version":"1.0",
    "event_id":"stable-id",
    "session_id":"agentd-logical-session",
    "generation":1,
    "sequence":42,
    "timestamp":"2026-08-09T12:34:56.000000000Z",
    "event_type":"tool.call",
    "stream":"provider_stdout",
    "payload":{},
    "raw_base64":"exact-provider-or-runtime-bytes",
    "raw_sha256":"hex-digest"
  },
  "context":{
    "job_id":"caller-idempotency-key",
    "agent":"codex",
    "runtime":"docker",
    "request_manifest":{},
    "secret_grants":["OPENAI_API_KEY"],
    "provider_session_id":"provider-thread-id",
    "image_reference":"image:tag",
    "image_digest":"sha256:digest",
    "sandbox_profile":"docker-native-v1"
  }
}
```

`request_manifest` is the durable, secret-scrubbed admission manifest.
`secret_grants` contains names only. Secret values are never delivered from
AgentD's durable store. `raw_base64` preserves JSON and non-JSON records byte
for byte; adapters should retain raw evidence before deriving trace views.

The adapter returns only after it has made the event replay-safe:

```json
{
  "type":"ack",
  "delivery_id":"same-as-event-id",
  "status":"accepted",
  "event_id":"stable-id",
  "session_id":"agentd-logical-session",
  "sequence":42,
  "trace_id":"851ad0da-3f90-4ea8-9094-9b644d1913f7"
}
```

`status` is `accepted`, `duplicate`, or `rejected`. AgentD advances the
checkpoint only for `accepted` or `duplicate` with the exact delivery ID,
event ID, session ID, and sequence. `trace_id` must be a stable UUID and cannot
change for a session. The adapter must deduplicate by event ID and/or
`(session_id, sequence)` before returning `duplicate`.

## Replay and storage

AgentD's SQLite event ledger is the source buffer. Checkpoints live at:

```text
~/.agentd/plugins/<plugin>/checkpoints/<session-id>.json
```

They are written atomically with private permissions. On AgentD or adapter
restart, delivery resumes strictly after the last acknowledged sequence.
AgentD first verifies the checkpoint's event identity against the ledger. A
future, corrupt, mismatched, or missing-range checkpoint never advances
silently.

The external system must make its own event handling idempotent. This closes
the crash window where OpenTraces accepted an event but AgentD died before
publishing the new checkpoint: AgentD redelivers the same identity, the adapter
returns `duplicate`, and the checkpoint then advances without a second trace
effect.

## Health and shutdown

AgentD sends `health`, `flush`, and `shutdown` control frames containing a
`request_id`. The response repeats the type and request ID with `status:"ok"`.
These are adapter lifecycle operations only; none affect an agent runtime.

Inspect runtime state with:

```sh
curl -sS http://127.0.0.1:8090/api/v1/plugins
curl -sS http://127.0.0.1:8090/api/v1/sessions/<session-id>/traces
```

The first endpoint reports configured adapter version, default policy,
healthy/degraded/down state, last error, and unacknowledged event count. The
second returns AgentD session to external trace UUID linkage and its last
acknowledged sequence.

## OpenTraces boundary

The OpenTraces adapter should be maintained with the OpenTraces capture layer,
where agent-specific `SessionParser` and `FormatImporter` integrations are
defined. AgentD deliberately does not write OpenTraces bucket files or call
private Python APIs. Until OpenTraces publishes a stable generic live-ingest
command, the separately versioned adapter is the compatibility boundary.

Remote synchronization is outside this protocol. AgentD emits no sync,
publish, authentication, or authorization message. Any OpenTraces security,
redaction, review, and remote publishing policy remains configured and
enforced by OpenTraces.
