# agy (jetski / Google Antigravity CLI) — adapter findings

**Status: NO ADAPTER LANDED.** Two of the three blockers from the 2026-07-20
GameDevTests audit are now solved (recipes below), but headless print mode
still cannot be bound to a real workspace — tool writes land in the CLI's
internal scratch directory, never in the working directory. Until that is
fixed (or a project-binding flag is found), agy cannot run benchmark/coding
sessions through agentruntime.

Probed 2026-07-21 against `agy 1.1.4` on macOS. All experiments used a fake
HOME; the real `~/.gemini`, `~/.tessera` were read, never modified.

## Failure matrix

| # | Failure (2026-07-20 audit) | Status 2026-07-21 | Fix / detail |
|---|---------------------------|-------------------|--------------|
| 1 | Real HOME headless: any tool use dies on `a tool required the "mcp" permission that headless mode cannot prompt for` even with `--dangerously-skip-permissions` | **SOLVED** (workaround) | `--dangerously-skip-permissions` is broken in 1.1.4 — the auto-deny error text *suggests the very flag being passed*. The real lever is `permissions.allow` in `~/.gemini/antigravity-cli/settings.json` (path found via binary strings; not documented in `--help`). Enumerate the permission types — `"*"` is NOT a catch-all: `{"permissions":{"allow":["mcp","read_file","write_file","edit_file","command","browser"]}}` — with that list, headless tool use proceeds with no prompt. |
| 2 | Fake HOME: auth survives, model list works, but sessions land in an onboarding wizard, ignore `--model`, never use tools | **SOLVED** | The wizard trigger is missing CLI state, not missing auth. A working fake HOME contains: `.gemini/{oauth_creds.json,google_accounts.json,installation_id}`, `.gemini/antigravity-cli/{settings.json,installation_id,jetski_state.pbtxt,bin/,builtin/}`, plus `Library/Keychains` symlinked to the real keychain dir. With those copied, headless sessions authenticate and run tools directly — no wizard. |
| 3 | (new) Print mode ignores the working directory | **BLOCKING** | With permissions fixed, tools run — but every file write lands in `~/.gemini/antigravity-cli/scratch/`, not the CWD. Tried: CWD alone; `trustedWorkspaces:[<dir>]`; `git init` in the workspace; `--add-dir <dir>`; `--new-project`. None bind the workspace. `--add-dir`/`--new-project` runs also detour into the built-in `antigravity-guide` skill and stall past the print timeout. Print mode appears to run against an internal ephemeral workspace unless an existing *project* (IDE concept, `--project <id>`) is attached. |

## What a future adapter needs

1. **Workspace binding.** Find how print mode attaches to a real directory.
   Candidates not yet exhausted: pre-creating a project row (projects are
   managed in `~/.tessera/global.db` / `.gemini/projects.json`) and passing
   `--project <id>`; or driving the interactive TUI under a PTY with scripted
   input (`--prompt-interactive` + expect-style automation). The PTY route
   was not completed inside the timebox — permission and onboarding fixes
   consumed it; nothing suggests the PTY changes the scratch-workspace
   behavior, since interactive mode uses the same project model.
2. **Isolation recipe** (ready once #1 lands): the fake HOME from failure #2
   contains no user MCP config, no GEMINI.md, no skills/knowledge dirs.
   Residual would be the CLI's built-in skills (e.g. `antigravity-guide`) —
   same class as claude/codex/grok built-ins. Probe before first scored run.
3. **Bug to watch upstream:** `--dangerously-skip-permissions` not honored in
   headless print mode (1.1.4). If Google fixes it, the settings.json
   enumeration becomes unnecessary. Re-check on each agy release —
   `permissions.allow` semantics (bare name vs `name(<target>)`) may also
   drift.

## Isolation notes (for when an adapter lands)

- Auth: Google OAuth — `oauth_creds.json` + keyring entries (keychain on
  macOS). Both the JSON copy AND the keychain symlink were present in the
  working fake HOME; which one is load-bearing was not isolated.
- Context surfaces to strip: `~/.gemini/GEMINI.md`, `~/.gemini/skills/`,
  `.gemini/antigravity-cli/{knowledge,brain,conversations,mcp,implicit}/`.
  The working fake HOME simply omits them all.
- `~/.tessera/global.db` copy was included in the fake HOME during testing;
  not yet verified whether it is required (it may only matter for the
  Tessera code-index features).
