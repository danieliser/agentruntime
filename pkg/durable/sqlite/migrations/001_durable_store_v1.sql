-- DUR-102: first reviewed AgentD durable-store schema.
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
    session_id              TEXT NOT NULL,
    generation              INTEGER NOT NULL CHECK (generation > 0),
    runtime                 TEXT NOT NULL,
    state                   TEXT NOT NULL CHECK (state IN (
        'starting', 'running', 'exited', 'lost', 'indeterminate'
    )),
    container_id            TEXT NOT NULL UNIQUE,
    image_reference         TEXT NOT NULL,
    image_digest            TEXT NOT NULL,
    sandbox_profile         TEXT NOT NULL,
    provider_id             TEXT NOT NULL DEFAULT '',
    docker_log_driver       TEXT NOT NULL DEFAULT '',
    docker_log_options_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(docker_log_options_json)),
    created_at_ns           INTEGER NOT NULL,
    updated_at_ns           INTEGER NOT NULL CHECK (updated_at_ns >= created_at_ns),
    PRIMARY KEY (session_id, generation),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT
);

CREATE TABLE events (
    session_id     TEXT NOT NULL,
    sequence       INTEGER NOT NULL CHECK (sequence > 0),
    event_id       TEXT NOT NULL UNIQUE,
    generation     INTEGER NOT NULL CHECK (generation > 0),
    schema_version TEXT NOT NULL,
    timestamp_ns   INTEGER NOT NULL,
    type           TEXT NOT NULL,
    stream         TEXT NOT NULL CHECK (stream IN (
        'provider_stdout', 'runtime_stderr', 'control', 'lifecycle', 'terminal'
    )),
    payload_json   TEXT CHECK (payload_json IS NULL OR json_valid(payload_json)),
    raw            BLOB NOT NULL,
    raw_sha256     TEXT NOT NULL,
    PRIMARY KEY (session_id, sequence),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT,
    FOREIGN KEY (session_id, generation)
        REFERENCES runtime_generations(session_id, generation) ON DELETE RESTRICT
);

CREATE TABLE terminal_receipts (
    session_id    TEXT PRIMARY KEY,
    generation    INTEGER NOT NULL CHECK (generation > 0),
    state         TEXT NOT NULL CHECK (state IN (
        'completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate'
    )),
    exit_code     INTEGER,
    signal        TEXT NOT NULL DEFAULT '',
    started_at_ns INTEGER NOT NULL,
    ended_at_ns   INTEGER NOT NULL CHECK (ended_at_ns >= started_at_ns),
    output_hash   TEXT NOT NULL,
    artifact_hash TEXT NOT NULL DEFAULT '',
    last_sequence INTEGER NOT NULL CHECK (last_sequence >= 0),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT,
    FOREIGN KEY (session_id, generation)
        REFERENCES runtime_generations(session_id, generation) ON DELETE RESTRICT
);

CREATE INDEX events_session_generation_idx ON events(session_id, generation, sequence);
CREATE INDEX generations_session_state_idx ON runtime_generations(session_id, state);

CREATE TRIGGER events_no_update
BEFORE UPDATE ON events BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TRIGGER events_no_delete
BEFORE DELETE ON events BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TRIGGER receipts_no_update
BEFORE UPDATE ON terminal_receipts BEGIN
    SELECT RAISE(ABORT, 'terminal receipts are immutable');
END;

CREATE TRIGGER receipts_no_delete
BEFORE DELETE ON terminal_receipts BEGIN
    SELECT RAISE(ABORT, 'terminal receipts are immutable');
END;

CREATE TRIGGER sessions_identity_immutable
BEFORE UPDATE ON sessions
WHEN NEW.id <> OLD.id
  OR NEW.idempotency_key <> OLD.idempotency_key
  OR NEW.request_hash <> OLD.request_hash
  OR NEW.request_manifest_json <> OLD.request_manifest_json
  OR NEW.secret_grants_json <> OLD.secret_grants_json
  OR NEW.agent <> OLD.agent
  OR NEW.runtime <> OLD.runtime
  OR NEW.created_at_ns <> OLD.created_at_ns
BEGIN
    SELECT RAISE(ABORT, 'session identity is immutable');
END;

CREATE TRIGGER sessions_terminal_immutable
BEFORE UPDATE ON sessions
WHEN OLD.state IN ('completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate')
 AND NEW.state <> OLD.state
BEGIN
    SELECT RAISE(ABORT, 'terminal session state is immutable');
END;

CREATE TRIGGER generations_identity_immutable
BEFORE UPDATE ON runtime_generations
WHEN NEW.session_id <> OLD.session_id
  OR NEW.generation <> OLD.generation
  OR NEW.runtime <> OLD.runtime
  OR NEW.container_id <> OLD.container_id
  OR NEW.image_reference <> OLD.image_reference
  OR NEW.image_digest <> OLD.image_digest
  OR NEW.sandbox_profile <> OLD.sandbox_profile
  OR NEW.provider_id <> OLD.provider_id
  OR NEW.docker_log_driver <> OLD.docker_log_driver
  OR NEW.docker_log_options_json <> OLD.docker_log_options_json
  OR NEW.created_at_ns <> OLD.created_at_ns
BEGIN
    SELECT RAISE(ABORT, 'generation identity is immutable');
END;

CREATE TRIGGER generations_terminal_immutable
BEFORE UPDATE ON runtime_generations
WHEN OLD.state IN ('exited', 'lost', 'indeterminate')
 AND NEW.state <> OLD.state
BEGIN
    SELECT RAISE(ABORT, 'terminal generation state is immutable');
END;
