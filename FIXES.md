# FIXES — disposition of REVIEW.md findings

Branch `task/harness-refresh-2026-07`, fixes applied 2026-07-21.
Verification: `go build ./...` clean; `go test ./...` green (929 tests, 17
packages); backend tests additionally run under `-race`; all branch-touched
Go files `gofmt`-clean. Live checks against installed CLIs noted per finding.

| # | Severity | Finding | Disposition | Commit |
| --- | --- | --- | --- | --- |
| 1 | P0 | Clean codex exposes full sidecar env | **Fixed** | e27b6ea |
| 2 | P1 | Interactive codex flags invalid | **Fixed + live-verified** | 3458d33 |
| 3 | P1 | Claude clean context not an ephemeral home | **Fixed (env) / documented (home, by design)** | e27b6ea, 57d4848 |
| 4 | P1 | Clean context incomplete across session paths | **Fixed (chat/validation) / partially deferred (docker images)** | 8ecdcd4 |
| 5 | P1 | Cursor usage not normalized | **Fixed (duration_ms per-tool: not available)** | 506f23c |
| 6 | P1 | Grok/cursor double-close panic race | **Fixed (claude too)** | 36eab44 |
| 7 | P1 | Prompts logged in plaintext argv | **Fixed** | e27b6ea |
| 8 | P2 | Codex clean home stranded on spawn failure | **Fixed (SIGKILL residual remains, inherent)** | e84f696 |
| 9 | P2 | Isolation probes can false-pass | **Fixed** | 6d9f519 |
| 10 | P3 | gofmt / trailing whitespace | **Fixed (root cause: CRLF blobs)** | 57d4848 |

---

## 1 — P0: os.Environ() merged into clean-mode codex (FIXED)

Confirmed exactly as reported: the spawner merged the full host environment
whenever `spawnEnv` returned anything (which clean mode always did).

- `cmd/sidecar/codex.go:174` — spawner now sets `cmd.Env = env` verbatim,
  never `append(os.Environ(), ...)`.
- `cmd/sidecar/codex.go:1008` — `spawnEnv` builds the complete env from the
  allowlist builders for BOTH clean and regular sessions (regular codex
  previously inherited the whole host env too).
- `cmd/sidecar/env.go:38,49` — shared `buildCleanEnv` / `buildCleanContextEnv`
  allowlists (extracted from claude.go); `CODEX_HOME` added to the non-clean
  passthrough so relocated host roots keep working.
- Tests: `TestSpawnCodexAppServer_NoHostEnvMerge` (real spawner, `/usr/bin/env`,
  proves no merge — the gap the review noted in fake-spawner coverage),
  `TestCodexPromptMode_HostEnvAllowlist`, plus secret-leak assertions added to
  `TestCodexPromptMode_CleanHomeMaterialization`.

## 2 — P1: codex app-server rejects --model/--sandbox (FIXED, live-verified)

Reproduced before fixing, per instructions: on codex 0.144.6,
`codex app-server --model X` and `--sandbox Y` both exit 2 with "unexpected
argument". `codex exec` DOES accept both flags — exec mode is untouched.

- `cmd/sidecar/codex.go:361,364` — interactive spawn now uses
  `-c sandbox_mode=danger-full-access` and `-c model=<m>`.
- Live-verified: a JSON-RPC `initialize` against the real
  `codex app-server --listen stdio:// -c model=gpt-5.5 -c sandbox_mode=danger-full-access`
  returns the userAgent result (0.144.6, this machine, today).
- Test: `TestCodexInteractiveMode_ModelUsesConfigOverride`.

## 3 — P1: claude clean context / shared env passthrough (FIXED env, home documented)

Two claims here; they got different treatment:

- **Env leak — fixed.** Clean-context sessions for ALL adapters now use the
  strict allowlist (`cmd/sidecar/env.go:49`) that drops `XDG_CONFIG_HOME`,
  `XDG_DATA_HOME` (host config rediscovery), `NODE_OPTIONS`, `NODE_PATH`
  (code injection into node-based CLIs), `NVM_DIR`, and host `CODEX_HOME`.
  Wired at `cmd/sidecar/claude.go:360` and the grok/cursor/codex spawn paths.
- **Ephemeral home for claude — disputed, documented instead.** BRIEF Leg A
  prescribes flag-based isolation for claude and records that fake-HOME-class
  approaches (`--bare`) break subscription OAuth; `--safe-mode` is the route
  that probed clean on 2.1.216 with OAuth surviving. Switching claude to an
  ephemeral home would trade a probe-verified clean route for an unprobed one
  that likely breaks auth. What WAS wrong is the CHANGELOG claiming "every
  adapter materializes a minimal ephemeral home" — the docs now state
  claude's actual mechanism (57d4848). If a future probe shows `--safe-mode`
  leaking, ephemeral-home-with-keychain-symlink is the fallback to test.

## 4 — P1: clean context incomplete across session paths (FIXED core, docker gap deferred)

- `pkg/api/handlers.go:644` — `validateContextMode()` shared by both paths;
  the chat/internal `SpawnSession` previously validated nothing.
- `pkg/api/handlers.go:593` — `SpawnSession` now populates contamination
  (`:203` is the HTTP path).
- Chats can express context: `ChatConfig.Context` (`pkg/chat/types.go:37`),
  `ChatAPIConfig.Context` (`pkg/api/schema/types.go:340`), forwarded in
  `pkg/chat/manager.go` `spawnSession`. Adjacent bug found while there: the
  chat path stored `Effort` only as a display tag and never put it on the
  SessionRequest, so it never reached the sidecar — also fixed.
- `local-pipe` + `context:"clean"` is now rejected with a clear error instead
  of silently bypassing isolation.
- Docker + grok/cursor + clean is rejected up front (the agent image bundles
  neither CLI nor credentials — confirmed in `docker/Dockerfile.agent`).
- **Deferred:** adding grok/cursor binaries + credential materialization to
  the Docker image is real feature work (image build, auth plumbing for
  keychain-based cursor in a Linux container — cursor auth may not be
  portable at all), not a review fix. The rejection + docs make the gap
  explicit instead of failing mysteriously at container start.
- Tests: `pkg/api/context_mode_test.go` (validation matrix, SpawnSession
  contamination, HTTP 400, chat config round-trip).

## 5 — P1: cursor usage/tool_result not normalized (FIXED, one caveat)

- `cmd/sidecar/cursor.go:418` — `normalizeCursorUsage` renames camelCase keys
  (`inputTokens` → `input_tokens`, cache variants included) with unknown keys
  passed through. This also fixes the startup_crash misclassification, which
  keyed off tokens parsing as zero.
- `cmd/sidecar/cursor.go:408` — tool_result now carries `is_error`, derived
  from cursor's `result.{success|error}` payload shape.
- **Caveat (not fixable here):** cursor's stream emits no per-tool duration,
  so `duration_ms` on tool_result stays absent. The session-level result
  event does carry `duration_ms` and always did.
- Tests: normalized-usage and camelCase-absence assertions in
  `TestCursorBackend_EventMapping`; new `TestCursorBackend_ToolResultError`.

## 6 — P1: double-close panic race (FIXED, claude included)

- `grok.go:268`, `cursor.go:272` — `markDone()` guarded by `doneOnce`
  (`sync.Once`), called from both `Close()` and `waitForExit()`.
- `claude.go:492` — the claude backend had the identical unguarded
  select-then-close in `Stop()`/`waitForExit()`; fixed alongside though the
  review flagged only grok/cursor.
- Tests: `TestGrokBackend_CloseRacesNaturalExit`,
  `TestCursorBackend_CloseRacesNaturalExit` (50 iterations each), suite run
  under `-race`.
- **Adjacent risk noted, not fixed:** all prompt-mode backends close the
  events channel in `waitForExit` while reader goroutines may still be
  draining buffered stdout lines; a mid-drain emit racing that close is a
  theoretical send-on-closed-channel panic that predates this branch and
  spans every backend. Fixing it properly means restructuring channel
  ownership — flagged for follow-up rather than rushed here.

## 7 — P1: prompts logged in plaintext (FIXED)

- `cmd/sidecar/env.go:81` — `redactPromptArgs` replaces any argv element
  equal to the prompt with `[prompt: N bytes]`; used at `grok.go:169`,
  `cursor.go:174`, and `claude.go:363` (claude's `-p` mode logged the prompt
  too — same class, fixed though unflagged).
- `pkg/agent/redact.go` + `pkg/api/handlers.go:253` — same treatment for the
  daemon's session-spawn log.
- Codex never logged argv; the sidecar and runtimes log `AGENT_PROMPT` nowhere.

## 8 — P2: stranded clean CODEX_HOME (FIXED, inherent residual documented)

- `cmd/sidecar/codex.go` — `removeCleanHome()` (`:541` helper) now runs on
  spawner failure in both exec (`:265`) and app-server (`:376`) paths, and in
  `Close()`.
- Test: `TestCodexPromptMode_SpawnFailureRemovesCleanHome`.
- **Residual:** a sidecar killed with SIGKILL cannot run cleanup — any
  /tmp-materialization scheme shares that hole. Mitigations (OS temp
  reaping; a daemon-side sweep of `agentruntime-*-home-*` orphans) are noted
  as follow-up; the latter belongs in the runtime, not the sidecar.

## 9 — P2: probe false-passes (FIXED)

- `pkg/e2e/probeassert.go:41` — `evaluateProbeAnswer` is structure-aware: an
  answer must be NONE or a bulleted/numbered list whose EVERY item matches an
  accepted residual; unstructured non-NONE answers fail; hard tells fail
  anywhere. Lives outside the e2e build tag so `go test ./...` covers it
  (regression test included for the "default rules to help you" false-pass
  the review described). Cursor's overly broad `"rules"` residual tightened.
- **Partially addressed:** the review also wanted probes exercising HTTP,
  chat, Docker, local-pipe, and session metadata. Session-metadata and
  path-validation coverage landed as unit tests (finding 4); local-pipe is
  now rejected rather than probed; full HTTP/chat/Docker *live probe*
  variants would each spend real LLM calls per CI run and need the daemon +
  Docker image in the loop — deferred as e2e follow-up rather than done
  badly here.

## 10 — P3: formatting (FIXED, root cause identified)

The four Go files and the CHANGELOG section were committed with **CRLF line
endings** — the `\r` is what `git diff --check` flagged on every line, and
gofmt treats CRLF Go files as unformatted. All branch-touched files are now
LF and gofmt-clean (57d4848). Note: many files unrelated to this branch are
still CRLF on main (`gofmt -l` lists ~50); normalizing the repo plus adding
a `.gitattributes` (`* text=auto eol=lf`) is recommended as a separate
mechanical PR so this branch's diff stays reviewable.

---

## Not part of the findings, observed while fixing

- `TestConcurrentDoubleDelete_NoPanic` (pkg/api) flaked once during the run
  (two concurrent DELETEs both returned success; expected exactly one).
  Pre-existing — reproduced 0/10 on retry, unrelated to these changes, but
  it suggests the delete handler's remove path isn't atomic. Worth its own
  look.
