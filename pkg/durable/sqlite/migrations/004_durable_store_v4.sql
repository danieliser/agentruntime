-- Reviewed continuation plan: CONTINUATION.md. Preserve legacy rows and
-- immutable proof; no table or receipt is deleted.
ALTER TABLE terminal_receipts RENAME TO terminal_receipts_v3;
CREATE TABLE terminal_receipts (
    session_id    TEXT NOT NULL,
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
    terminal_reason TEXT NOT NULL DEFAULT '' CHECK (terminal_reason IN (
        '', 'completed', 'failed', 'cancelled', 'terminated',
        'timed_out', 'crashed', 'indeterminate'
    )),
    PRIMARY KEY (session_id, generation),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT,
    FOREIGN KEY (session_id, generation)
        REFERENCES runtime_generations(session_id, generation) ON DELETE RESTRICT
);

INSERT INTO terminal_receipts (session_id, generation, state, exit_code, signal,
 started_at_ns, ended_at_ns, output_hash, artifact_hash, last_sequence, terminal_reason)
SELECT session_id, generation, state, exit_code, signal, started_at_ns,
 ended_at_ns, output_hash, artifact_hash, last_sequence, terminal_reason
FROM terminal_receipts_v3;

CREATE TRIGGER generation_receipts_no_update
BEFORE UPDATE ON terminal_receipts BEGIN
 SELECT RAISE(ABORT, 'terminal receipts are immutable');
END;
CREATE TRIGGER generation_receipts_no_delete
BEFORE DELETE ON terminal_receipts BEGIN
 SELECT RAISE(ABORT, 'terminal receipts are immutable');
END;
DROP TRIGGER sessions_terminal_immutable;
CREATE TRIGGER sessions_terminal_immutable
BEFORE UPDATE ON sessions
WHEN OLD.state IN ('completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate')
 AND NEW.state <> OLD.state
 AND NOT (OLD.state = 'completed' AND NEW.state = 'starting')
BEGIN
 SELECT RAISE(ABORT, 'terminal session state is immutable');
END;
