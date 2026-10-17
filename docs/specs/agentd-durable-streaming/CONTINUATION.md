# Follow-up continuation (A1–A5)

The minimal lifecycle change is an explicit `completed → starting → running`
transition. `completed` still means the current execution finished, so existing
wait/receipt consumers keep their completion boundary. Cancelled (including
terminated), failed, timed out, crashed and indeterminate sessions cannot wake.
The prior generation is never reopened. Follow-up input uses the same native
resume implementation as `/resume`, with the stored manifest and provider ID.

Schema v4 plan: replace the session-only receipt key with `(session_id,
generation)`, copying existing rows without changing their contents. Rename
the old table to `terminal_receipts_v3` and retain it as immutable legacy proof;
no data or tables are deleted. Relax only the completed-to-starting lifecycle
trigger. Exercise upgrades and receipt immutability on isolated fixtures before
using this schema with real data. This change does not migrate installed data.

`GET /api/v1/sessions/:id` adds `continuable` and `continuable_reason`.
`continuable` describes whether a new prompt is currently admissible: an idle
transport accepts a warm prompt; a completed session with retained native state
wakes a new generation. A confirmed-lost generation can also wake if its durable
ledger proves its last turn completed with no later input intent; unfinished
lost turns still require explicit recovery. Missing provider state, secret grants
requiring explicit regrant, unavailable runtimes and disposed ephemeral
workspaces fail closed.

Receipts are immutable per generation. `/receipt` selects the active generation;
`?generation=N` reads historical proof. `/result` follows the same generation
selection (current output can be read before receipt finalization) and never
returns a previous generation's artifact as a new generation's result.
Event sequences remain contiguous across the logical session. Prompt wake control
events bridge the prior generation to its replacement; its receipt freezes the
earlier output boundary. Exact control retries are read-only, including after
completion or restart. An intent without dispatch proof stays indeterminate
and is never automatically replayed into a paid provider. If a wake fails before
a new runtime generation is recorded, admission stays unresolved and further
prompts are blocked; the prior receipt remains readable by generation.

Input and explicit resume share admission and serialization. A prompt during
an active turn reports `invalid_state` and points to `steer`. The maintained
Docker lease keeps its existing timer and warm-input behavior; it reduces
latency but is not needed for follow-up continuation.

Diagnostic generation one retains `{id}.ndjson`; subsequent generations write
`{id}.gN.ndjson`, so a new turn cannot alter the file hashed by an earlier
receipt. Recovered generations append only within their own diagnostic file.

Verification uses deterministic Claude/Codex protocol fixtures and a runtime
boundary fixture for Docker, with real local subprocesses and SQLite databases.
This verifies generation admission, native resume wire messages, saved settings,
idempotency, and receipt/event consistency; it is not live-provider acceptance.
