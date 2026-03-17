# agentruntime — Deep Code Exploration Summary

> Produced 2026-03-17 for spec-writer consumption.
> Covers: API, Bridge, Session, Credentials, Materializer, Agent, SDK layers.

---

## 1. API Layer (`pkg/api/`)

### 1.1 Server Structure

`Server` struct (`server.go:19`) holds:

| Field      | Type                | Purpose                                     |
|------------|---------------------|---------------------------------------------|
| `router`   | `*gin.Engine`       | HTTP router (gin, release mode, recovery middleware) |
| `sessions` | `*session.Manager`  | Thread-safe session registry                |
| `runtime`  | `runtime.Runtime`   | The single active runtime (local or docker) |
| `agents`   | `*agent.Registry`   | Agent name → Agent implementation map       |
| `dataDir`  | `string`            | Root for session state, credentials, logs   |
| `logDir`   | `string`            | NDJSON log file directory (default `./logs`) |

`ServerConfig` optionally sets `DataDir` and `LogDir`. `LogDir` defaults to `"./logs"`; `DataDir` defaults to `filepath.Dir(logDir)`.

HTTP server uses `ReadTimeout: 30s`, `WriteTimeout: 30s`.

### 1.2 Endpoints

| Method | Path                    | Handler                  | Request Type      | Response Type       | Notes |
|--------|-------------------------|--------------------------|-------------------|---------------------|-------|
| GET    | `/health`               | `handleHealth`           | —                 | `HealthResponse`    | Returns `{"status":"ok","runtime":"<name>"}` |
| POST   | `/sessions`             | `handleCreateSession`    | `SessionRequest`  | `SessionResponse`   | 201 Created. Validates agent+prompt, spawns process, attaches IO |
| GET    | `/sessions`             | `handleListSessions`     | —                 | `[]SessionSummary`  | Sorted by CreatedAt, then ID |
| GET    | `/sessions/:id`         | `handleGetSession`       | —                 | `Session` snapshot  | Raw session snapshot (includes all JSON-tagged fields) |
| GET    | `/sessions/:id/info`    | `handleGetSessionInfo`   | —                 | `SessionInfo`       | Rich response with ws_url, log_url, session_dir, log_file |
| GET    | `/sessions/:id/logs`    | `handleGetLogs`          | `?cursor=<int64>` | `text/plain`        | Replay buffer poll. Returns `Agentruntime-Log-Cursor` header |
| GET    | `/sessions/:id/log`     | `handleGetLogFile`       | —                 | `application/x-ndjson` | Serves the full persistent NDJSON log file |
| DELETE | `/sessions/:id`         | `handleDeleteSession`    | —                 | `{"id","state"}`    | Kills process, sets completed(-1), removes from manager |
| GET    | `/ws/sessions/:id`      | `handleSessionWS`        | `?since=<int64>`  | WebSocket upgrade   | Bridge connection. 409 if no active handle |

### 1.3 Error Handling

- 400: missing agent, missing prompt (non-interactive), unknown agent, unknown runtime, invalid cursor, resume errors
- 404: session not found, log file not found
- 409: session has no active process (WS upgrade rejected)
- 500: session dir prep failure, spawn failure, log file lookup error
- 503: max sessions limit reached (`ErrMaxSessions`)

### 1.4 Session Creation Flow (`handleCreateSession`)

1. Bind JSON → `SessionRequest`
2. Validate: agent required, prompt required (unless interactive)
3. Validate runtime name matches server's runtime (if specified)
4. `EffectiveMounts()` → resolve WorkDir into mounts list
5. `effectiveWorkDir()` → pick WorkDir from request or first rw mount
6. Look up agent in registry
7. `lookupResumeSessionID()` → resolve resume_session for claude/codex
8. `ag.BuildCmd(prompt, agCfg)` → build CLI argv
9. Docker special case: `spawnCmd = []string{cmd[0]}` (sidecar handles the rest)
10. `session.NewSession()` → create session in Pending state
11. `prepareSessionDir()` → for local runtime, init agent-specific session dirs
12. `sessions.Add()` → register (may fail with ErrMaxSessions)
13. `runtime.Spawn()` → create process
14. `sess.SetRunning(handle)` → transition to Running
15. Close stdin for non-interactive (prompt-mode) sessions
16. `AttachSessionIO()` → start drain goroutines + exit watcher
17. Return 201 with `SessionResponse`

### 1.5 Session IO (`sessionio.go`)

`AttachSessionIO(sess, logDir)`:
- Creates `LogWriter` (NDJSON file at `{logDir}/{sessionID}.ndjson`)
- Creates `DrainWriter` = `io.MultiWriter(replay, logw)` — tees to both
- Starts goroutines: drain stdout, drain stderr (via `drainTo`)
- Exit watcher goroutine: waits on `handle.Wait()`, then `drainWg.Wait()`, closes replay + log, calls `sess.SetCompleted(code)`

`drainTo()` reads in 32KB chunks from `io.ReadCloser` and writes to the multi-writer.

### 1.6 Type Aliases (`types.go`)

All schema types are re-exported from `pkg/api/schema` as type aliases:
`SessionRequest`, `Mount`, `ClaudeConfig`, `CodexConfig`, `MCPServer`, `ContainerConfig`, `HealthResponse`, `SessionResponse`, `SessionSummary`, `SessionInfo`, `Resources` (deprecated alias for `ContainerConfig`).

### 1.7 WebSocket Upgrader

- `ReadBufferSize: 4096`, `WriteBufferSize: 4096`
- `CheckOrigin: func(*http.Request) bool { return true }` — accepts all origins
- Bridge gets `context.WithTimeout(context.Background(), 24*time.Hour)`

---

## 2. SessionRequest (`pkg/api/schema/types.go`)

### 2.1 All Fields

| Field           | Type               | JSON/YAML tag                    | Default       | Semantics |
|-----------------|--------------------|----------------------------------|---------------|-----------|
| `TaskID`        | `string`           | `task_id,omitempty`              | `""`          | Caller-provided task identity for correlation |
| `Name`          | `string`           | `name,omitempty`                 | `""`          | Human label for observability |
| `Tags`          | `map[string]string` | `tags,omitempty`                | `nil`         | Arbitrary key-value metadata, cloned into session |
| `Agent`         | `string`           | `agent`                          | **required**  | Agent name: `"claude"`, `"codex"`, `"opencode"` |
| `Runtime`       | `string`           | `runtime,omitempty`              | server default | `"local"` or `"docker"` |
| `Model`         | `string`           | `model,omitempty`                | `""`          | Cross-agent model override (e.g. `"claude-opus-4-5"`) |
| `Prompt`        | `string`           | `prompt`                         | **required*** | The initial user prompt (*not required if `Interactive`) |
| `Timeout`       | `string`           | `timeout,omitempty`              | `"5m"`        | Go duration string parsed by `EffectiveTimeout()` |
| `PTY`           | `bool`             | `pty,omitempty`                  | `false`       | Allocate PTY for interactive agents |
| `Interactive`   | `bool`             | `interactive,omitempty`          | `false`       | Keep stdin open, steer via WS frames |
| `ResumeSession` | `string`           | `resume_session,omitempty`       | `""`          | agentruntime session ID to resume |
| `WorkDir`       | `string`           | `work_dir,omitempty`             | `""`          | Shorthand → `Mount{Host: val, Container: "/workspace", Mode: "rw"}` |
| `Mounts`        | `[]Mount`          | `mounts,omitempty`               | `nil`         | Explicit bind-mounts |
| `Claude`        | `*ClaudeConfig`    | `claude,omitempty`               | `nil`         | Claude-specific config (only read if Agent=claude) |
| `Codex`         | `*CodexConfig`     | `codex,omitempty`                | `nil`         | Codex-specific config (only read if Agent=codex) |
| `MCPServers`    | `[]MCPServer`      | `mcp_servers,omitempty`          | `nil`         | MCP servers injected into agent config at spawn |
| `Env`           | `map[string]string` | `env,omitempty`                 | `nil`         | Clean-room env vars for container (never inherits host env) |
| `Container`     | `*ContainerConfig` | `container,omitempty`            | `nil`         | Image, resource limits, network, security |

### 2.2 Sub-types

**Mount**: `Host string`, `Container string`, `Mode string` ("rw"|"ro")

**ClaudeConfig**:

| Field             | Type               | Semantics |
|-------------------|--------------------|-----------|
| `SettingsJSON`    | `map[string]any`   | → `~/.claude/settings.json` |
| `ClaudeMD`        | `string`           | → `~/.claude/CLAUDE.md` |
| `McpJSON`         | `map[string]any`   | → `~/.claude/.mcp.json` (merged with MCPServers) |
| `CredentialsPath` | `string`           | Host path to `credentials.json` (bind-mounted ro) |
| `MemoryPath`      | `string`           | Host path to `~/.claude/projects/{hash}/` (bind-mounted ro) |
| `OutputFormat`    | `string`           | Default `"stream-json"` |

**CodexConfig**:

| Field          | Type               | Semantics |
|----------------|--------------------|-----------|
| `ConfigTOML`   | `map[string]any`   | → `~/.codex/config.toml` |
| `Instructions` | `string`           | → `~/.codex/instructions.md` |
| `ApprovalMode` | `string`           | `"auto-edit"` / `"suggest"` / `"full-auto"` |

**MCPServer**: `Name`, `Type` ("http"|"stdio"|"websocket"), `URL` (supports `${HOST_GATEWAY}`), `Cmd []string`, `Env map[string]string`, `Token`

**ContainerConfig**: `Image` (default "ubuntu:22.04"), `Memory` (e.g. "4g"), `CPUs` (float64), `Network` (default "bridge"), `SecurityOpt []string`. Default security: `--cap-drop ALL --cap-add DAC_OVERRIDE --security-opt no-new-privileges:true`

### 2.3 Methods

**`EffectiveMounts()`**: Returns new slice. If `WorkDir != ""`, prepends `Mount{Host: WorkDir, Container: "/workspace", Mode: "rw"}`, then appends all explicit `Mounts`. Does not mutate original.

**`EffectiveTimeout()`**: Parses `Timeout` as `time.Duration`. Returns parsed value or `5 * time.Minute` if empty/unparseable.

### 2.4 Response Types

**`SessionResponse`** (POST /sessions): `SessionID`, `TaskID`, `Agent`, `Runtime`, `Status`, `WSURL`, `LogURL`

**`SessionSummary`** (GET /sessions): adds `CreatedAt time.Time`, `Tags map[string]string`

**`SessionInfo`** (GET /sessions/:id/info): adds `EndedAt *time.Time`, `ExitCode *int`, `SessionDir string`, `LogFile string`

---

## 3. Bridge Layer (`pkg/bridge/`)

### 3.1 Overview

`Bridge` connects a session's `ReplayBuffer` to a WebSocket client. It never reads process pipes directly — it subscribes to the replay buffer via `WaitFor()`.

Data flow: `process → drain goroutine → ReplayBuffer → Bridge.replayStreamPump → WS client`

### 3.2 Constants

| Constant       | Value  |
|----------------|--------|
| `pingInterval` | 30s    |
| `pongTimeout`  | 10s    |
| `writeTimeout` | 5s     |
| `readTimeout`  | 60s    |

### 3.3 ServerFrame (server → client)

| Field       | Type     | JSON tag                | Used in frame types |
|-------------|----------|-------------------------|---------------------|
| `Type`      | `string` | `type`                  | ALL: `stdout`, `stderr`, `exit`, `replay`, `connected`, `pong`, `error` |
| `Data`      | `string` | `data,omitempty`        | `stdout`, `replay` — base64 for binary, plain for valid UTF-8 |
| `ExitCode`  | `*int`   | `exit_code,omitempty`   | `exit` |
| `Offset`    | `int64`  | `offset,omitempty`      | `stdout`, `replay` — replay buffer byte offset |
| `SessionID` | `string` | `session_id,omitempty`  | `connected` |
| `Mode`      | `string` | `mode,omitempty`        | `connected` — always `"pipe"` currently |
| `Error`     | `string` | `error,omitempty`       | `error` |

### 3.4 ClientFrame (client → server)

| Field     | Type                   | JSON tag               | Used in frame types |
|-----------|------------------------|------------------------|---------------------|
| `Type`    | `string`               | `type`                 | `stdin`, `ping`, `resize`, `steer`, `interrupt`, `context`, `mention` |
| `Data`    | `string`               | `data,omitempty`       | `stdin`, `steer` |
| `Cols`    | `int`                  | `cols,omitempty`       | `resize` |
| `Rows`    | `int`                  | `rows,omitempty`       | `resize` |
| `Context` | `*ClientFrameContext`  | `context,omitempty`    | `context` — `{Text, FilePath}` |
| `Mention` | `*ClientFrameMention`  | `mention,omitempty`    | `mention` — `{FilePath, LineStart, LineEnd}` |

### 3.5 Bridge.Run() Flow

1. Create cancellable context (from caller's ctx)
2. If `sinceOffset >= 0`:
   - `replay.ReadFrom(sinceOffset)` → send `replay` frame (base64-encoded)
   - Set `streamOffset` to next offset
3. Else: `streamOffset = replay.TotalBytes()` (no replay)
4. Send `connected` frame with session ID and mode
5. Set pong handler (resets read deadline to `readTimeout`)
6. Start 3 concurrent goroutines:
   - **pingLoop**: sends WS Ping every 30s
   - **replayStreamPump**: blocks on `replay.WaitFor(offset)`, sends `stdout` frames (UTF-8 check: plain text or base64), sends `exit` frame when buffer closes
   - **stdinPump** (on main goroutine): reads `ClientFrame` JSON, routes by type

### 3.6 Client Frame Routing (stdinPump)

| Frame type  | Action |
|-------------|--------|
| `stdin`     | Write `frame.Data` to `handle.Stdin()` |
| `steer`     | `SteerableHandle.SendSteer(data)` — error if not steerable |
| `interrupt`  | `SteerableHandle.SendInterrupt()` |
| `context`   | `SteerableHandle.SendContext(text, filePath)` |
| `mention`   | `SteerableHandle.SendMention(filePath, lineStart, lineEnd)` |
| `ping`      | Reply with `pong` ServerFrame |
| `resize`    | TODO: forward to PTY |
| unknown     | Reply with `error` frame: `"unknown frame type: <type>"` |

### 3.7 Replay-on-Reconnect

Query param `?since=<offset>` on `/ws/sessions/:id`. If provided and >= 0, bridge reads replay buffer from that offset and sends a `replay` frame before streaming. The `offset` field in every `stdout`/`replay` frame lets clients track position for reconnection.

### 3.8 Steerable Handle

The bridge checks if the `ProcessHandle` satisfies `runtime.SteerableHandle` (type assertion). If not, returns `runtime.ErrNotSteerable`. The `SteerableHandle` interface adds: `SendPrompt`, `SendInterrupt`, `SendSteer`, `SendContext`, `SendMention`.

---

## 4. Session Layer (`pkg/session/`)

### 4.1 Session Struct

| Field         | Type                    | JSON tag               | Notes |
|---------------|-------------------------|------------------------|-------|
| `ID`          | `string`                | `id`                   | UUID v4, generated by `NewSession` |
| `TaskID`      | `string`                | `task_id,omitempty`    | Caller-provided correlation ID |
| `AgentName`   | `string`                | `agent_name`           | |
| `RuntimeName` | `string`                | `runtime_name`         | |
| `SessionDir`  | `string`                | `session_dir,omitempty`| Host path to materialized agent home |
| `Tags`        | `map[string]string`     | `tags,omitempty`       | Cloned on creation |
| `State`       | `State`                 | `state`                | Lifecycle enum |
| `ExitCode`    | `*int`                  | `exit_code,omitempty`  | Set by `SetCompleted` |
| `CreatedAt`   | `time.Time`             | `created_at`           | Set at construction |
| `EndedAt`     | `*time.Time`            | `ended_at,omitempty`   | Set by `SetCompleted` |
| `Replay`      | `*ReplayBuffer`         | `-` (not serialized)   | Lazy-allocated (1 MiB default) |
| `Handle`      | `runtime.ProcessHandle` | `-` (not serialized)   | Set by `SetRunning` |
| `mu`          | `sync.Mutex`            | (unexported)           | Guards state transitions |

### 4.2 State Enum

| Value         | Meaning |
|---------------|---------|
| `pending`     | Created, not yet spawned |
| `running`     | Process is active |
| `completed`   | Process exited with code 0 |
| `failed`      | Process exited with code != 0 |
| `orphaned`    | Recovered from previous daemon run |

### 4.3 State Transitions

- `NewSession()` → `pending`
- `SetRunning(handle)` → `running` (attaches handle)
- `SetCompleted(0)` → `completed` (sets ExitCode + EndedAt)
- `SetCompleted(non-zero)` → `failed`
- `Recover()` → `orphaned`

### 4.4 Snapshot

`Snapshot()` returns a copy of all value fields under lock. Used before JSON serialization to avoid races with concurrent `SetCompleted`. Does NOT copy `Replay` or `Handle`.

### 4.5 Manager

Thread-safe registry using `sync.RWMutex`. Map of `string → *Session`.

| Method          | Behavior |
|-----------------|----------|
| `Add(s)`        | Checks max sessions limit, checks ID uniqueness |
| `Get(id)`       | RLock read |
| `Remove(id)`    | Lock delete |
| `List()`        | RLock, returns slice of all sessions |
| `SetMaxSessions(n)` | 0 = unlimited |
| `ShutdownAll()` | Kill all, close replays, set completed(-1), clear map |
| `Recover(handles, runtimeName)` | Creates orphaned sessions from recovered handles |

### 4.6 ReplayBuffer (`replay.go`)

Bounded circular ring buffer. Default capacity: **1 MiB** (`1 << 20`).

| Method              | Semantics |
|---------------------|-----------|
| `Write(p)`          | Append to ring, overwrite oldest. Broadcasts to WaitFor callers |
| `WriteOffset(p)`    | Same as Write, also returns new total offset atomically |
| `ReadFrom(offset)`  | Returns `(data, nextOffset)`. If offset too old → reads from oldest available. If caught up → returns `(nil, total)` |
| `WaitFor(offset)`   | **Blocks** until data beyond offset exists or buffer is closed. Returns `(data, nextOffset, done)` |
| `TotalBytes()`      | Monotonic total bytes ever written |
| `Close()`           | Sets `done=true`, broadcasts all waiters |
| `IsDone()`          | Check if closed |
| `LoadFromFile(path)`| Stream file into buffer via `io.Copy` |

Lazy allocation: `newLazyReplayBuffer` defers `make([]byte, size)` until first write via `ensureBufferLocked()`.

Concurrency: `sync.Mutex` + `sync.Cond` (broadcast on every write and close).

### 4.7 LogWriter (`logfile.go`)

Persistent NDJSON log files.

- File extension: `.ndjson` (legacy: `.jsonl`)
- Path: `{logDir}/{sessionID}.ndjson`
- Opened in append mode (`O_CREATE|O_WRONLY|O_APPEND`, mode 0644)
- `DrainWriter(replay, logw)` → `io.MultiWriter(replay, logw)` — tees process output to both replay buffer and log file
- `ExistingLogFilePath()` checks both `.ndjson` and `.jsonl` extensions for backward compat

### 4.8 Agent Sessions (`pkg/session/agentsessions/`)

**Claude** (`claude.go`):
- `InitClaudeSessionDir(dataDir, sessionID, projectPath, credentialsPath)` → creates `{dataDir}/claude-sessions/{sessionID}/` with subdirs `projects/{mangled-path}/` and `sessions/`
- Copies credentials to both `credentials.json` and `.credentials.json` (legacy compat)
- Copies host `~/.claude.json` account state to skip onboarding
- `MangleProjectPath(absPath)` → replaces `/` with `-`
- `ReadLastClaudeSessionID(sessionDir)` → reads `sessions/*.json` (sorted by `startedAt`), fallback to newest `.jsonl` by mtime
- `ClaudeResumeArgs()` → `["--resume", "--session-id", "<id>"]`

**Codex** (`codex.go`):
- `InitCodexSessionDir(dataDir, sessionID)` → creates `{dataDir}/codex-sessions/{sessionID}/sessions/`
- `ReadLastCodexSessionID(sessionDir)` → walks `sessions/` tree, reads `{id}` from JSON or falls back to filename
- `CodexResumeArgs()` → `["--session", "<id>"]`

**Pruning**: `PruneOldSessions(dataDir, retention)` sweeps both `claude-sessions/` and `codex-sessions/`, removes dirs older than retention by mtime.

---

## 5. Credentials Layer (`pkg/credentials/`)

### 5.1 Sync Struct

| Field       | Type              | Purpose |
|-------------|-------------------|---------|
| `dataDir`   | `string`          | Base directory (e.g. `~/.local/share/agentruntime`) |
| `extractor` | `tokenExtractor`  | Platform-specific credential extraction |
| `mu`        | `sync.Mutex`      | Serializes credential operations |

### 5.2 tokenExtractor Interface

```go
type tokenExtractor interface {
    Extract(service string) (string, error)
}
```

### 5.3 Platform Implementations

| Platform | Build tag         | Implementation        | Mechanism |
|----------|-------------------|-----------------------|-----------|
| macOS    | `darwin`          | `keychainExtractor`   | `security find-generic-password -s <service> -w` |
| Linux    | `linux`           | `linuxExtractor`      | 1) `secret-tool lookup service <service>` (GNOME/KDE), 2) fallback to cached file at `{dataDir}/credentials/claude-credentials.json` |
| Other    | `!darwin && !linux`| `stubExtractor`       | Returns error — manual placement required |

Linux `dataDir` defaults to `$XDG_DATA_HOME` or `~/.local/share/agentruntime`.

### 5.4 Claude Credentials

`ClaudeCredentialsFile()`:
- Service name: `"Claude Code-credentials"`
- Cache file: `{dataDir}/credentials/claude-credentials.json`
- Cache TTL: **30 seconds** (`throttleInterval`)
- If cache is fresh, returns immediately
- If extraction fails but stale cache exists, returns stale cache
- Writes with mode `0600`

### 5.5 Codex Credentials

`CodexCredentialsFile()`: Checks `~/.codex/auth.json`. No extraction — expects file to exist.

`CodexAPIKey()`: Checks `OPENAI_API_KEY` env, falls back to `ANTHROPIC_API_KEY`.

### 5.6 Watch Goroutine

`Watch(ctx, interval)`: Background goroutine that calls `ClaudeCredentialsFile()` on a ticker. Best-effort — errors ignored. Cancel via context.

---

## 6. Materializer (`pkg/materialize/`)

### 6.1 Result Struct

| Field        | Type              | Purpose |
|--------------|-------------------|---------|
| `SessionDir` | `string`          | Host path to materialized agent home |
| `Mounts`     | `[]apischema.Mount` | Additional mounts for the container |
| `CleanupFn`  | `func()`          | Removes temp dir (when dataDir is empty) |

### 6.2 `Materialize(req, sessionID, dataDir)` Flow

1. If `dataDir == ""`: create temp dir `os.MkdirTemp("", "agentruntime-<prefix>")`; CleanupFn = `os.RemoveAll`
2. If `req.Claude != nil || req.Agent == "claude"` → `materializeClaude()`
3. If `req.Codex != nil || req.Agent == "codex"` → `materializeCodex()`

### 6.3 Claude Materialization

Files written to `{claudeDir}/`:

| File               | Source                          | Notes |
|--------------------|---------------------------------|-------|
| `settings.json`    | `ClaudeConfig.SettingsJSON`     | Auto-injects `skipDangerousModePermissionPrompt: true` |
| `CLAUDE.md`        | `ClaudeConfig.ClaudeMD`         | Written as plain text |
| `.mcp.json`        | `ClaudeConfig.McpJSON` merged with `MCPServers` | MCP server configs merged; URLs sanitized (http/https/ws/wss only); `${HOST_GATEWAY}` resolved |
| `.claude.json`     | Hardcoded trust state           | Pre-trusts `/workspace`, skips onboarding, auto-updates disabled |

Mounts produced:

| Order | Host                          | Container                                    | Mode |
|-------|-------------------------------|----------------------------------------------|------|
| 1     | `{claudeDir}`                 | `/home/agent/.claude`                        | rw   |
| 2     | `{claudeDir}/.claude.json`    | `/home/agent/.claude.json`                   | rw   |
| 3*    | `{memoryPath}`                | `/home/agent/.claude/projects/{sha256[:16]}/` | ro   |

*Mount 3 only if `MemoryPath` is set.

Credential auto-discovery (when `CredentialsPath` is not explicitly set):
1. Check `{dataDir}/credentials/claude-credentials.json` (sync cache)
2. Check `~/.claude/.credentials.json` and `~/.claude/credentials.json` on host

### 6.4 Codex Materialization

Files written to `{codexDir}/`:

| File              | Source                        | Notes |
|-------------------|-------------------------------|-------|
| `config.toml`     | `CodexConfig.ConfigTOML`      | Flat TOML marshaler + hardcoded `[projects."/workspace"] trust_level = "trusted"` |
| `instructions.md` | `CodexConfig.Instructions`    | Written as plain text |
| `auth.json`       | Auto-discovered               | Priority: sync cache, then `~/.codex/auth.json` |

Mounts produced:

| Host          | Container              | Mode |
|---------------|------------------------|------|
| `{codexDir}`  | `/home/agent/.codex`   | rw   |

### 6.5 HOST_GATEWAY Resolution (`gateway.go`)

`ResolveHostGateway()`:

| Platform | Resolution |
|----------|-----------|
| macOS    | `host.docker.internal` |
| Linux    | Parse `/proc/net/route` for default gateway IP; fallback `172.17.0.1` |
| Other    | `host.docker.internal` |

`ResolveVars(s)` replaces `${HOST_GATEWAY}` in any string with the resolved gateway address.

### 6.6 MCP Config Merging

`buildClaudeMCPJSON()`:
1. Deep-clone base `McpJSON`
2. Extract or create `mcpServers` map
3. For each `MCPServer`, convert to map and add to `mcpServers`
4. Sanitize all values: URLs validated (http/https/ws/wss only; empty → removed), tokens have control chars stripped

### 6.7 Security Helpers

- `sanitizeSessionID()`: only allows `[a-zA-Z0-9_-]`, replaces dots and slashes with `-`, trims leading/trailing `-`
- `stripRelativeTraversal()`: removes leading `../` sequences after `filepath.Clean`
- `expandPath()`: supports `~`, env vars, resolves relative to CWD

---

## 7. Agent Interface (`pkg/agent/`)

### 7.1 Agent Interface

```go
type Agent interface {
    BuildCmd(prompt string, cfg AgentConfig) ([]string, error)
    Name() string
    ParseOutput(output []byte) (*AgentResult, bool)
}
```

### 7.2 AgentConfig

| Field             | Type               | Semantics |
|-------------------|--------------------|-----------|
| `Model`           | `string`           | Model override |
| `MaxTokens`       | `int`              | Response length limit |
| `WorkDir`         | `string`           | Process working directory |
| `SessionID`       | `string`           | agentruntime session ID |
| `ResumeSessionID` | `string`           | Resume a prior agent-native session |
| `Interactive`     | `bool`             | Keep stdin open |
| `AllowedTools`    | `[]string`         | Tool restrictions |
| `Env`             | `map[string]string` | Additional env vars |

### 7.3 AgentResult

| Field      | Type               | Semantics |
|------------|--------------------|-----------|
| `Summary`  | `string`           | Human-readable summary |
| `ExitCode` | `int`              | Agent-reported exit status |
| `Metadata` | `map[string]any`   | Agent-specific structured data |

### 7.4 Registry

`DefaultRegistry()` pre-registers `ClaudeAgent` and `CodexAgent`. OpenCodeAgent exists but is NOT registered by default.

### 7.5 ClaudeAgent

**BuildCmd**: `claude --dangerously-skip-permissions [-p <prompt>] --output-format stream-json --verbose [--model X] [--max-turns N] [--resume --session-id X] [--allowedTools X ...]`

- Prompt required unless interactive
- Interactive mode omits `-p` flag
- `MaxTokens` maps to `--max-turns` (naming quirk)
- Resume: `ResumeSessionID` preferred, falls back to `SessionID`

**ParseOutput**: Scans NDJSON for `{"type":"result",...}`. Extracts:
- `result` field → `Summary`
- `subtype`: `"success"` → exit 0, else → exit 1
- `cost_usd`, `duration_ms` → `Metadata`

### 7.6 CodexAgent

**BuildCmd**:
- Prompt mode: `codex exec --json --full-auto --skip-git-repo-check <prompt> [--model X] [--session X]`
- Interactive: `codex --no-alt-screen [--model X] [--session X]`

**ParseOutput**: Scans NDJSON for `message.completed` (reads `content`) or `response.completed` (navigates `response.output[].content[].text`).

### 7.7 OpenCodeAgent

**BuildCmd**: `opencode run <prompt> [--model X]`. Prompt always required.

**ParseOutput**: Stub — returns `nil, false`. Marked TODO.

---

## 8. Runtime Interface (`pkg/runtime/runtime.go`)

### 8.1 Runtime Interface

```go
type Runtime interface {
    Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error)
    Recover(ctx context.Context) ([]ProcessHandle, error)
    Name() string
    Cleanup(ctx context.Context) error
}
```

### 8.2 SpawnConfig

| Field        | Type                       | Purpose |
|--------------|----------------------------|---------|
| `SessionID`  | `string`                   | Container naming/labels |
| `AgentName`  | `string`                   | Agent type identifier |
| `Cmd`        | `[]string`                 | Command + args |
| `Prompt`     | `string`                   | For runtimes using control channels |
| `Env`        | `map[string]string`        | Process env |
| `WorkDir`    | `string`                   | Working directory |
| `TaskID`     | `string`                   | Task identifier |
| `Request`    | `*apischema.SessionRequest`| Full request for materialization |
| `SessionDir` | `*string`                  | Output: host path to materialized session files |
| `PTY`        | `bool`                     | Pseudo-terminal allocation |

### 8.3 ProcessHandle Interface

| Method         | Return type         | Notes |
|----------------|---------------------|-------|
| `Stdin()`      | `io.WriteCloser`    | Write to process stdin |
| `Stdout()`     | `io.ReadCloser`     | Read process stdout |
| `Stderr()`     | `io.ReadCloser`     | nil if PTY (merged) |
| `Wait()`       | `<-chan ExitResult`  | Blocks until exit |
| `Kill()`       | `error`             | Immediate termination |
| `PID()`        | `int`               | 0 for remote runtimes |
| `RecoveryInfo()`| `*RecoveryInfo`    | nil for non-recovered handles |

### 8.4 SteerableHandle (extends ProcessHandle)

| Method           | Params                                  | Purpose |
|------------------|-----------------------------------------|---------|
| `SendPrompt`     | `content string`                        | Sidecar prompt command |
| `SendInterrupt`  | —                                       | Sidecar interrupt command |
| `SendSteer`      | `content string`                        | Mid-conversation steering |
| `SendContext`     | `text, filePath string`                 | Attach context |
| `SendMention`    | `filePath string, lineStart, lineEnd int`| Reference file location |

### 8.5 ExitResult

`Code int`, `Err error`, `ErrorDetail string` (from sidecar exit frame).

### 8.6 RecoveryInfo

`SessionID string`, `TaskID string` — used during orphan recovery.

---

## 9. SDK (`sdk/`)

Both `sdk/python/README.md` and `sdk/node/README.md` are **placeholder stubs**. They state:

- SDKs will be auto-generated from OpenAPI spec once API stabilizes
- Planned: `AgentRuntimeClient` (HTTP), `SessionStream` (WebSocket)
- Node: TypeScript types from OpenAPI
- Python: async support via asyncio
- Current guidance: use raw HTTP + WebSocket clients

No actual SDK code exists.

---

## 10. Cross-Cutting Observations

### Thread Safety
- Session state transitions use `sync.Mutex` per session
- Manager uses `sync.RWMutex` for registry operations
- ReplayBuffer uses `sync.Mutex` + `sync.Cond` for producer/consumer coordination
- Bridge uses `sync.Mutex` for WebSocket writes (gorilla/websocket is not concurrent-write-safe)
- Credentials Sync uses `sync.Mutex` for cache access

### Data Flow (complete path)
```
Client POST /sessions
  → SessionRequest validation
  → Agent.BuildCmd() → CLI argv
  → Runtime.Spawn() → ProcessHandle
  → Session.SetRunning(handle)
  → AttachSessionIO():
      stdout/stderr → drainTo() → MultiWriter(ReplayBuffer, LogWriter)
      exit watcher → Wait() → drainWg.Wait() → Close replay → SetCompleted

Client WS /ws/sessions/:id?since=N
  → Bridge.Run():
      ReadFrom(since) → replay frame
      replayStreamPump: WaitFor(offset) loop → stdout frames → exit frame
      stdinPump: ClientFrame routing → handle.Stdin() or SteerableHandle methods
```

### Credential Flow
```
Option A: explicit CredentialsPath in ClaudeConfig
Option B: credentials.Sync.Watch() → Keychain/secret-tool → cache file → auto-discovered by materializer
Option C: host ~/.claude/.credentials.json auto-discovered
```

### Session Directory Layout (persistent, with dataDir)
```
{dataDir}/
  claude-sessions/{sessionID}/
    settings.json
    CLAUDE.md
    .mcp.json
    .claude.json
    credentials.json
    .credentials.json
    projects/{mangled-path}/
    sessions/
  codex-sessions/{sessionID}/
    config.toml
    instructions.md
    auth.json
    sessions/
  credentials/
    claude-credentials.json
    codex-auth.json
  logs/
    {sessionID}.ndjson
```
