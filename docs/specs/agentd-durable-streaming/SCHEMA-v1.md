# AgentD durable store v1 — Gate G1 review sheet

Status: **APPROVED AND IMPLEMENTED**

Date: 2026-08-09

Task IDs: DUR-101, DUR-102, DUR-103, DUR-104, DUR-105

## Review decision

Approved one AgentD-owned SQLite database with four domain tables:

1. `sessions` — logical identity, idempotency, reconstructable request manifest,
   lifecycle state, active generation, and durable event tail;
2. `runtime_generations` — Docker/container identity and resolved runtime profile;
3. `events` — append-only AgentD envelopes with exact native record bytes; and
4. `terminal_receipts` — immutable terminal proof committed atomically with the
   final session and generation transitions.

Approval was granted on 2026-08-09. Migration `001_durable_store_v1.sql`, the
SQLite implementation, integrity checks, snapshot metadata, and daemon-owned
store initialization are implemented. Public v1 routes and removal of the
current sidecar transport remain gated separately.

## Driver and file

- Driver: `modernc.org/sqlite` pinned at `v1.46.1`, the newest release compatible
  with this repository's Go 1.24 baseline at implementation time.
- Reason: pure Go keeps AgentD cross-compilation and static packaging free of a
  new CGO/toolchain requirement.
- Path: `${AGENTRUNTIME_DATA_DIR}/agentd.sqlite`; the data root defaults to
  `~/.agentd` when no explicit flag or environment override is supplied.
- Directory mode: `0700`; database and backup mode: `0600`.
- One store is opened during daemon startup and injected through
  `durable.Store`; no scattered database opens or SQL.

The complete default persistence layout is kept under one relocatable root:

```text
~/.agentd/
├── agentd.sqlite
├── backups/          # SQLite snapshots plus .metadata.json manifests
├── chats/            # named chat JSON records
├── logs/             # legacy/current session history during migration
└── credentials and reconstructed runtime homes/configuration
```

## Representation rules

- IDs, hashes, states, event types, and stream names: `TEXT`.
- Exact provider/runtime record: `BLOB`; never decoded and re-encoded for
  storage.
- Derived payload and manifests: canonical UTF-8 JSON `TEXT` with
  `json_valid(...)` checks.
- Timestamps: UTC Unix nanoseconds in `INTEGER`; API serialization remains
  RFC3339Nano.
- Sequences and generation numbers: positive signed 64-bit integers.
- SHA-256 values: lowercase `sha256:<hex>` text for operational readability.
- Booleans: constrained `INTEGER` values `0` or `1`.

## Secret and request-manifest rule

`request_manifest_json` contains the resolved, reconstructable, non-secret
session configuration. It may include environment variable names and ordinary
non-secret values, but never credential values.

Secrets use explicit grant references. `secret_grants_json` records grant names
and optional opaque version references; it never records resolved values.
Runtime generation `N+1` must reacquire those grants or return a structured
failure—it cannot reuse logged environment values.

`request_hash` is SHA-256 over canonical `request_manifest_json` plus canonical
secret grant references. Therefore duplicate-create comparison does not hash or
persist secret values.

The compatibility adapter must split the existing generic `env` map into
ordinary environment and explicit secret grants before it calls the v1 store.
Until that classification exists, public v1 idempotent creation is not enabled.

## Approved tables

The canonical executable form is
`pkg/durable/sqlite/migrations/001_durable_store_v1.sql`.

```sql
CREATE TABLE sessions (
    id                    TEXT PRIMARY KEY,
    idempotency_key       TEXT NOT NULL UNIQUE,
    request_hash          TEXT NOT NULL,
    request_manifest_json TEXT NOT NULL CHECK (json_valid(request_manifest_json)),
    secret_grants_json    TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(secret_grants_json)),
    agent                 TEXT NOT NULL,
    runtime               TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN (
        'created', 'starting', 'running', 'completed', 'failed',
        'cancelled', 'timed_out', 'crashed', 'indeterminate'
    )),
    active_generation     INTEGER NOT NULL DEFAULT 0 CHECK (active_generation >= 0),
    last_sequence         INTEGER NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
    created_at_ns         INTEGER NOT NULL,
    updated_at_ns         INTEGER NOT NULL CHECK (updated_at_ns >= created_at_ns)
);

CREATE TABLE runtime_generations (
    session_id             TEXT NOT NULL,
    generation             INTEGER NOT NULL CHECK (generation > 0),
    runtime                TEXT NOT NULL,
    state                  TEXT NOT NULL CHECK (state IN (
        'starting', 'running', 'exited', 'lost', 'indeterminate'
    )),
    container_id           TEXT NOT NULL UNIQUE,
    image_reference        TEXT NOT NULL,
    image_digest           TEXT NOT NULL,
    sandbox_profile        TEXT NOT NULL,
    provider_id            TEXT NOT NULL DEFAULT '',
    docker_log_driver      TEXT NOT NULL DEFAULT '',
    docker_log_options_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(docker_log_options_json)),
    created_at_ns          INTEGER NOT NULL,
    updated_at_ns          INTEGER NOT NULL CHECK (updated_at_ns >= created_at_ns),
    PRIMARY KEY (session_id, generation),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT
);

CREATE TABLE events (
    session_id      TEXT NOT NULL,
    sequence        INTEGER NOT NULL CHECK (sequence > 0),
    event_id        TEXT NOT NULL UNIQUE,
    generation      INTEGER NOT NULL CHECK (generation > 0),
    schema_version  TEXT NOT NULL,
    timestamp_ns    INTEGER NOT NULL,
    type            TEXT NOT NULL,
    stream          TEXT NOT NULL CHECK (stream IN (
        'provider_stdout', 'runtime_stderr', 'control', 'lifecycle', 'terminal'
    )),
    payload_json    TEXT CHECK (payload_json IS NULL OR json_valid(payload_json)),
    raw             BLOB NOT NULL,
    raw_sha256      TEXT NOT NULL,
    PRIMARY KEY (session_id, sequence),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT,
    FOREIGN KEY (session_id, generation)
        REFERENCES runtime_generations(session_id, generation) ON DELETE RESTRICT
);

CREATE TABLE terminal_receipts (
    session_id      TEXT PRIMARY KEY,
    generation      INTEGER NOT NULL CHECK (generation > 0),
    state           TEXT NOT NULL CHECK (state IN (
        'completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate'
    )),
    exit_code       INTEGER,
    signal          TEXT NOT NULL DEFAULT '',
    started_at_ns   INTEGER NOT NULL,
    ended_at_ns     INTEGER NOT NULL CHECK (ended_at_ns >= started_at_ns),
    output_hash     TEXT NOT NULL,
    artifact_hash   TEXT NOT NULL DEFAULT '',
    last_sequence   INTEGER NOT NULL CHECK (last_sequence >= 0),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT,
    FOREIGN KEY (session_id, generation)
        REFERENCES runtime_generations(session_id, generation) ON DELETE RESTRICT
);
```

## Immutability enforcement

Migration v1 will add triggers that:

- reject `UPDATE` and `DELETE` on `events`;
- reject `UPDATE` and `DELETE` on `terminal_receipts`;
- reject changes to session identity, idempotency key, request hash, manifest,
  secret grants, agent, runtime, or creation time;
- reject changes to generation identity, container identity, image identity,
  sandbox profile, log configuration, or creation time; and
- reject every session or generation transition out of a terminal state.

The application validates transitions too. Triggers protect integrity from
future code paths and operator SQL; they are not a replacement for typed domain
errors.

## Transaction contracts

### Idempotent create

One `BEGIN IMMEDIATE` transaction:

1. look up `idempotency_key`;
2. if found with the same request hash, return the existing row;
3. if found with a different hash, return `idempotency_conflict`;
4. otherwise insert the logical session; and
5. commit before runtime admission.

The session row is created before Docker starts. A crash after commit leaves a
discoverable `created` session, not an invisible paid process.

### Runtime generation creation

One `BEGIN IMMEDIATE` transaction:

1. reject a terminal logical session;
2. reject creation if the current generation is nonterminal;
3. allocate `active_generation + 1`;
4. insert the generation identity; and
5. update `sessions.active_generation`.

Docker creation and DB creation cannot be one transaction. Reconciliation uses
the committed generation record plus Docker labels; every crash boundary has an
explicit expected/missing/duplicate state handled by DKR-502.

### Event append

One `BEGIN IMMEDIATE` transaction:

1. if `event_id` exists with byte-identical immutable fields, return it;
2. if the same ID differs, return `immutable_conflict`;
3. reject terminal sessions/generations;
4. allocate `sessions.last_sequence + 1`;
5. insert the event and exact `raw` bytes;
6. update `sessions.last_sequence`; and
7. commit before publishing to subscribers.

### Terminal finalization

One `BEGIN IMMEDIATE` transaction performs all three writes:

1. compare the expected nonterminal session and generation states;
2. verify the receipt's last sequence equals `sessions.last_sequence`;
3. transition the generation to its terminal state;
4. transition the logical session to its terminal state;
5. insert the terminal receipt; and
6. commit.

An exact retry returns the existing receipt. A differing retry returns
`immutable_conflict`. There is no public receipt-write method separate from
session finalization, preventing terminal-without-receipt crash states.

## SQLite runtime settings

Applied once during store startup and verified:

```text
PRAGMA foreign_keys = ON
PRAGMA journal_mode = WAL
PRAGMA synchronous = FULL
PRAGMA busy_timeout = 5000
PRAGMA temp_store = MEMORY
```

The store uses bounded contexts on every operation, caps connections for the
single-writer model, and maps busy/constraint failures into structured errors.

## Integrity checks

Startup and explicit health checks run:

- `PRAGMA quick_check`;
- `PRAGMA foreign_key_check`;
- session tail validation: `count(events) == last_sequence` and
  `min(sequence) == 1` when nonempty;
- terminal session ↔ terminal receipt equality; and
- active generation ↔ highest generation equality.

Any failure degrades AgentD health and marks affected recovery results
`indeterminate`; AgentD does not silently repair or restart work.

## Backup and restore

- A consistent online snapshot uses SQLite's backup API or `VACUUM INTO` from
  the single store package after a WAL checkpoint.
- Snapshot destination must be a new `0600` file; existing backups are never
  overwritten in place.
- Backup metadata records schema version, creation time, database SHA-256, and
  last committed event sequence per active session.
- Qualification includes restoring a snapshot into a new data directory,
  running integrity checks, replaying terminal receipts, and reconstructing
  active Docker-generation metadata without starting containers.

## Completed migration sequence

1. Pinned the SQLite driver and added the central store configuration.
2. Added migration `001_durable_store_v1.sql` containing tables, indexes, and
   immutability triggers exactly once.
3. Implemented the SQLite store behind `durable.Store`.
4. Ran the same contract suite against the in-memory and SQLite stores.
5. Added restart, terminal receipt, corruption, backup, and restore tests.
6. Integrated the green store into daemon startup and injected it into the API
   server dependency graph; compatibility handlers still await migration.

## Compatibility boundaries

- Existing in-memory `session.Manager` remains until the daemon integration
  phase; it is not silently treated as durable.
- Existing NDJSON logs are legacy history, never imported as proven v1 events
  without an explicit verified importer.
- Existing `session_id` requests map to the v1 idempotency key only in the
  compatibility adapter. New v1 callers provide an explicit job key.
- Existing generic environment values require classification before v1
  persistence, as described in the secret rule above.
- Sidecar byte offsets never map to v1 event sequences.

## Gate G1 acceptance checklist

- [x] Four-table boundary accepted.
- [x] Pure-Go SQLite driver direction accepted.
- [x] Exact raw bytes plus derived JSON accepted.
- [x] Secret-grant/request-hash rule accepted.
- [x] Atomic finalization contract accepted.
- [x] Append-only triggers accepted.
- [x] Backup/restore approach accepted.
- [x] Migration creation authorized.
