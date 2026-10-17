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
a new runtime generation is recorded, an explicitly unstarted successor records
the failed admission. Session failure and its receipt commit atomically; prior
receipts remain unchanged and readable by generation. Cancel/terminate intent is
admissible while a starting session still points at an exited predecessor.
Startup reconciliation settles this crash window as indeterminate after confirmed
runtime discovery. Integrity checks flag it as `wake_recovery_required` before
reconciliation and must pass after recovery before the daemon serves traffic.

Input, explicit resume, stop and finalization serialize per session; a blocked
spawn or input send does not stall unrelated sessions. Recovered transports
restore idle from the durable turn boundary for both maintained and ordinary
sessions. Without a recovered transport, a durably idle lost generation uses
the wake path. The last-turn result is cached per generation and durable sequence
so repeated GETs do not rescan its history. Named chats steer a running turn
when ordinary prompt admission rejects it.

Input and explicit resume share admission and serialization. A prompt during
an active turn reports `invalid_state` and points to `steer`. The maintained
Docker lease keeps its existing timer and warm-input behavior; it reduces
latency but is not needed for follow-up continuation.

Diagnostic generation one retains `{id}.ndjson`; subsequent generations write
`{id}.gN.ndjson`, so a new turn cannot alter the file hashed by an earlier
receipt. Recovered generations append only within their own diagnostic file.
Byte-range and legacy chat readers concatenate all generations in numeric order,
with session-wide offsets and NDJSON preferred over JSONL for each generation.

Verification uses deterministic Claude/Codex protocol fixtures and a runtime
boundary fixture for Docker, with real local subprocesses and SQLite databases.
This verifies generation admission, native resume wire messages, saved settings,
idempotency, and receipt/event consistency; it is not live-provider acceptance.
