# Harness Refresh 2026-07 — Deliverable

Branch: `task/harness-refresh-2026-07`. Five commits, one per leg (A, B, C,
E, D-findings), plus this docs commit. Full `go test ./...` was green before
every commit (1104 tests, 16 packages, final run included). Live e2e and
isolation probes were run against the real CLIs on this machine on
2026-07-21; commands and results below.

## What landed

### Leg A — claude + codex adapter refresh (`ea0705c`) ✅
- Claude `--effort xhigh|max` auto-pairs `alwaysThinkingEnabled: true`
  settings (verified: xhigh 400s without it).
- Claude `--bare` fails fast without `ANTHROPIC_API_KEY` (bare skips
  OAuth/keychain entirely).
- Codex exec mode spawns with **no stdin pipe** — the process reads
  `/dev/null` unconditionally (`os/exec` wires the null device), closing the
  detached-codex-blocks-forever failure class structurally.
- Codex clean `CODEX_HOME` materialization (auth.json + minimal config.toml,
  removed on close).
- New request plumbing: `SessionRequest.effort`, `ClaudeConfig.system_prompt`,
  `CodexConfig.service_tier` → `AGENT_CONFIG` → sidecar.
- Test status: unit-tested (arg construction, settings pairing, clean-home
  materialization/teardown, stdin discipline); full suite green.

### Leg B — grok backend (`6a710bd`) ✅
- Single-turn prompt mode; wire format probed live (thought/text token
  deltas + `end` record). Normalized to the unified event schema; grok's
  `sessionId` becomes the session identity.
- Clean context = fake HOME with only `.grok/{auth.json,agent_id,config.toml}`
  (+`models_cache.json`), matching the GameDevTests-verified recipe. auth.json
  required (file-based auth); teardown on close.
- Live verification: `TestE2E_V2_GrokPromptMode` passed against grok 0.2.106.
- **Known gap:** grok streaming-json emits **no tool events** — tool activity
  is invisible between text deltas (probe-verified: file created, no event).

### Leg C — cursor backend (`c6316d4`) ✅
- Prompt mode with stream-json normalization including tool_use/tool_result
  (`editToolCall` → `edit`, etc.).
- Clean context = fake HOME + `Library/Keychains` symlink (auth is
  keychain-based; verified working). `~/.cursor` deliberately not copied.
- Live verification: `TestE2E_V2_CursorPromptMode` passed against
  cursor-agent 2026.07.17.
- **Known gap (unstrippable, surfaced not fought):** account-level user rules
  sync server-side — reported as `cursor-account-rules` contamination. The
  live probe confirmed the rules (including frontend-design preferences)
  inject even in clean mode.
- Effort has no CLI flag; encode in the model string (`m[effort=high]`).

### Leg E — clean-context sessions (`cf229fc`) ✅
- `context: "clean"` on `SessionRequest`, threaded to every adapter;
  auto-discovery forced off for clean sessions.
- `contamination` reported in three places: `SessionInfo`, sidecar `/health`,
  and a `system{subtype:"contamination"}` NDJSON event.
- Isolation self-tests (`pkg/e2e/cleancontext_probe_test.go`): each adapter
  launches its real CLI through the sidecar against the **real host HOME**
  and asks the model to enumerate its own context. **All four passed live on
  2026-07-21** (`go test -tags e2e ./pkg/e2e/ -run TestE2E_CleanContext`).
- **Significant empirical finding:** the 2026-07-20 verified claude isolation
  flag set is **no longer clean on claude 2.1.216** — re-probed today, it
  leaks plugin MCP servers, ~60 skills, and host CLAUDE.md. The adapter now
  uses `--safe-mode`, which probed clean (NONE) and keeps subscription OAuth
  (verified with no `ANTHROPIC_API_KEY` in env). GameDevTests' launch.sh
  claude route should be updated the same way.
- New residual discovered: codex 0.144 ships built-in skills (imagegen,
  skill-creator, …) that were not present at the 2026-03 audit; reported as
  `codex-builtin-skills`.

### Leg D — agy/jetski (`07537fb`) — findings doc, no adapter ⚠️
- Two of the three blockers are now **solved** (recipes in
  `docs/agy-jetski-notes.md`): headless permission auto-deny (enumerated
  `permissions.allow` in `~/.gemini/antigravity-cli/settings.json`;
  `--dangerously-skip-permissions` is broken in 1.1.4 and `"*"` is not a
  catch-all) and the fake-HOME onboarding wizard (copy oauth/state files +
  keychain symlink).
- **Remaining blocker:** print mode never binds the working directory — all
  tool writes land in the CLI's internal scratch dir. CWD, trustedWorkspaces,
  git init, `--add-dir`, `--new-project` all fail. Timeboxed per the brief;
  PTY-driving interactive mode was not completed and is unlikely to change
  the workspace model (documented in the notes).

## Test status per leg

| Leg | Unit | Live e2e | Isolation probe |
|-----|------|----------|-----------------|
| A claude/codex | ✅ green | ✅ (pre-existing claude/codex e2e paths; probes below) | ✅ claude, ✅ codex |
| B grok | ✅ green | ✅ TestE2E_V2_GrokPromptMode | ✅ grok |
| C cursor | ✅ green | ✅ TestE2E_V2_CursorPromptMode | ✅ cursor (account rules accepted residual) |
| E clean-context | ✅ green | — | ✅ all four |
| D agy | n/a | n/a — no adapter | n/a |

Full suite: `go test ./...` → 1104 tests, 16 packages, all green.
E2E (opt-in): `go test -tags e2e ./pkg/e2e/ -run 'TestE2E_V2_(Grok|Cursor)PromptMode|TestE2E_CleanContext'` → all green (costs a few small LLM calls).

## Known gaps and caveats (honest list)

1. **Docker clean-context is partial.** Clean sessions force auto-discovery
   off, and docker's materialized homes are already per-session — but no
   in-container isolation probe was run (needs the agent image rebuilt with
   grok/cursor CLIs installed, which doesn't exist yet). Local runtime is the
   probe-verified path.
2. **Docker images don't include grok/cursor.** The bundled
   `agentruntime-agent:latest` image installs claude and codex only; grok and
   cursor sessions are local-runtime-only until the image adds them.
3. **grok tool activity is invisible** (CLI limitation, not adapter): no
   tool events in streaming-json as of 0.2.106.
4. **grok/cursor are prompt-mode only** — no interactive/steering protocol
   exists for either CLI. Steer/context/mention commands return errors;
   interrupt kills the process.
5. **Probe assertions are heuristic.** The isolation self-tests assert on
   model-generated text (hard leak tells + NONE/accepted residuals). A wording
   change in a CLI's built-ins could need whitelist updates. This is inherent
   to the probe approach — config inspection is worse (it lies).
6. **Legacy `local-pipe` runtime untouched** — codex under local-pipe still
   gets a live stdin pipe (potential hang on current codex); the sidecar path
   is the supported route.
7. **Claude interactive (--ide) mode + clean context** is untested together —
   clean context was designed for and verified in prompt mode (the
   GameDevTests use case). The flag set applies in both modes but the IDE MCP
   handshake under `--safe-mode` was not probed.
8. **Chat API `effort`** still only tags sessions (`pkg/chat`); it is not yet
   forwarded into the new `SessionRequest.Effort` field.
9. **Leftover temp dir:** `/tmp/agy-fake-home-xCjn` from Leg D probing could
   not be `rm`'d under this session's permissions; all credential copies
   inside it were overwritten with empty JSON (scrubbed). Safe to delete.

## Follow-ups recommended

- Update GameDevTests `harness/launch.sh` claude route to `--safe-mode`
  (its verified flag set regressed on claude 2.1.216 — probe evidence above).
- Add grok + cursor to the Docker agent image, then run the isolation probes
  in-container.
- Re-check agy on each release: workspace binding is the only remaining
  blocker, and the `--dangerously-skip-permissions` bug may get fixed.
