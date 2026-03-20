# Backlog

## High Priority

### agentd-tui — interactive terminal client [EXPERIMENTAL — shipped v0]
Separate binary (`cmd/agentd-tui/`) using Bubble Tea + Glamour + Lipgloss. v0 is functional: connects via WS, renders streaming markdown, chat history on reconnect, auto-reconnect on dead sessions.

**Known gaps:**
- **AskUserQuestion handling**: Claude emits `tool_use` with `tool_name=AskUserQuestion` — detect it, render as interactive prompt, send response as tool_result. Currently `--dangerously-skip-permissions` auto-answers with empty string (known CC bug). Interactive TUI sessions may need a different permission mode.
- No collapsible tool sections
- Streaming delta → glamour re-render is periodic not incremental
- No visual distinction between "thinking" and "waiting for input" beyond the AskUserQuestion tool detection above

### Zero-downtime daemon updater
Binary hot-reload without dropping active sessions. Approach: SIGUSR2-triggered graceful restart — new process inherits the listen socket FD, old process drains existing connections. Running sidecar processes are unaffected (separate PIDs). New daemon re-discovers active sessions from the session registry or running containers.

Interim: `scripts/reinstall.sh` does build+install+restart with ~2s downtime.

### Docker session volumes
Named Docker volumes for session persistence across container restarts. Required for `--resume` to work in Docker mode (Claude's session state needs to survive container lifecycle).

### Codex resume support
Wire Codex `--session` flag for named session continuity. Similar to Claude `--resume` but different CLI syntax.

## Medium Priority

### Token usage estimation refinement
Current: heuristic cost estimation from token counts when agent doesn't report cost_usd. Needs: cache token pricing (cache_read is cheaper), model auto-detection from sidecar events, periodic pricing table updates.

### Named session IDs for Claude
Pass `--session-id` with a deterministic ID derived from chat name for session naming. Separate from `--resume` (which handles context continuity). Gives addressable sessions in Claude's session picker.

### Concurrent message queuing
Currently concurrent messages during active session are accepted but may race. Need proper queue with ordering guarantees.

### Graceful degradation for PERSIST dispatch
When agentd is temporarily unavailable (restart, crash), PERSIST dispatches should retry with backoff rather than failing immediately.

## Low Priority

### Chat export/import
Export chat history + config as portable format. Import to recreate a chat on another machine.

### Multi-runtime chat sessions
Allow a chat to switch runtimes (local → docker) between respawns without losing context.
