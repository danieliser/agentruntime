# agentruntime Runtime Layer — Spec-Ready Reference

> Generated from source. All names, signatures, and constants are exact quotes from the code.

---

## 1. Core Interfaces (`pkg/runtime/runtime.go`)

### `Runtime`

```go
type Runtime interface {
    Spawn(ctx context.Context, cfg SpawnConfig) (ProcessHandle, error)
    Recover(ctx context.Context) ([]ProcessHandle, error)
    Name() string
    Cleanup(ctx context.Context) error
}
```

| Method | Contract |
|--------|----------|
| `Spawn` | Creates a new agent process. Returns `ProcessHandle` for stdio interaction. |
| `Recover` | Finds orphaned sessions from a previous daemon run. Returns handles to them. |
| `Name` | Returns the runtime identifier string (`"local"`, `"docker"`). |
| `Cleanup` | Graceful teardown of runtime infrastructure (proxy containers, networks). Safe to call even if nothing was started. |

### `ProcessHandle`

```go
type ProcessHandle interface {
    Stdin() io.WriteCloser
    Stdout() io.ReadCloser
    Stderr() io.ReadCloser
    Wait() <-chan ExitResult
    Kill() error
    PID() int
    RecoveryInfo() *RecoveryInfo
}
```

| Method | Contract |
|--------|----------|
| `Stdin()` | Writer to process stdin. Returns `nil` for recovered handles without stdin. |
| `Stdout()` | Reader from process stdout. For WS handles, carries NDJSON event lines. |
| `Stderr()` | Reader from stderr. Returns `nil` when PTY or WS-based (stderr merged into structured events). |
| `Wait()` | Returns channel that receives `ExitResult` exactly once on termination. Safe to call multiple times — each caller gets its own channel (for `localHandle`). |
| `Kill()` | Terminates the process immediately. Behavior varies by implementation. |
| `PID()` | OS process ID. Returns `0` for remote/WS-based handles. |
| `RecoveryInfo()` | Returns `*RecoveryInfo` for recovered handles, `nil` otherwise. |

### `SteerableHandle`

```go
type SteerableHandle interface {
    ProcessHandle
    SendPrompt(content string) error
    SendInterrupt() error
    SendSteer(content string) error
    SendContext(text, filePath string) error
    SendMention(filePath string, lineStart, lineEnd int) error
}
```

Only `*wsHandle` implements `SteerableHandle`. Callers must type-assert:

```go
if steerable, ok := handle.(runtime.SteerableHandle); ok {
    steerable.SendPrompt("...")
}
```

Sentinel error: `var ErrNotSteerable = fmt.Errorf("handle does not support sidecar commands")`

### `ExitResult`

```go
type ExitResult struct {
    Code        int
    Err         error
    ErrorDetail string
}
```

- `Code`: Process exit code. `0` = success.
- `Err`: Error from Wait() mechanics, distinct from non-zero exit. `localHandle` treats non-zero exit as `code != 0, err == nil`.
- `ErrorDetail`: From sidecar exit frame's `error_detail` JSON field.

### `RecoveryInfo`

```go
type RecoveryInfo struct {
    SessionID string
    TaskID    string
}
```

### `SpawnConfig`

```go
type SpawnConfig struct {
    SessionID  string
    AgentName  string
    Cmd        []string
    Prompt     string
    Env        map[string]string
    WorkDir    string
    TaskID     string
    Request    *apischema.SessionRequest
    SessionDir *string
    PTY        bool
}
```

### `SpawnError`

```go
type SpawnError struct {
    Reason string
    Err    error
}
```

Format: `"spawn: <Reason>"` or `"spawn: <Reason>: <Err>"`. Implements `Unwrap() error`.

---

## 2. Runtime Implementations

### 2.1 `LocalRuntime` — CLI flag `"local-pipe"`

**Source:** `pkg/runtime/local.go`

**Struct:** `LocalRuntime struct{}` — zero fields, stateless.

**`Name()`** returns `"local"`.

> **Important:** Both `LocalRuntime` and `LocalSidecarRuntime` return `"local"` from `Name()`. The `agentd` CLI flag disambiguates: `--runtime local` → `LocalSidecarRuntime`, `--runtime local-pipe` → `LocalRuntime`.

**Spawn sequence:**

1. Validate `cfg.Cmd` non-empty.
2. `exec.CommandContext(ctx, cfg.Cmd[0], cfg.Cmd[1:]...)` with `cmd.Dir = cfg.WorkDir`.
3. `configureLocalProcessGroup(cmd)` — platform-specific (see §5).
4. `buildSpawnEnv(cfg.Env)` — merges extra vars onto `os.Environ()`. Returns `nil` when extra is empty (inherits parent env).
5. Opens three pipes: `cmd.StdinPipe()`, `cmd.StdoutPipe()`, `cmd.StderrPipe()`.
6. `cmd.Start()`.
7. Returns `*localHandle`.

**ProcessHandle implementation: `localHandle`**

```go
type localHandle struct {
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout io.ReadCloser
    stderr io.ReadCloser
    raw    chan ExitResult     // written once by Wait goroutine
    fanMu  sync.Mutex
    fanSub []chan ExitResult
    fanRes *ExitResult
}
```

| Method | Behavior |
|--------|----------|
| `Stdin()` | Returns stdin pipe. |
| `Stdout()` | Returns stdout pipe. |
| `Stderr()` | Returns stderr pipe (non-nil — this is pipe-mode, not WS). |
| `Wait()` | Fan-out: one goroutine calls `cmd.Wait()`, caches result, broadcasts to all subscriber channels. Each caller gets a new buffered channel. If already exited, delivers immediately. |
| `Kill()` | Calls `killLocalProcessGroup(cmd)` — sends `SIGKILL` to process group on Unix, `Process.Kill()` on non-Unix. |
| `PID()` | Returns `cmd.Process.Pid`. |
| `RecoveryInfo()` | Always `nil`. |

**Wait goroutine logic:**
- Calls `cmd.Wait()`.
- If error is `*exec.ExitError`: extracts exit code, sets `waitErr = nil` (non-zero exit is not an error).
- Sends `ExitResult{Code, Err}` to `raw` channel.
- `fanoutLoop()` drains `raw`, caches result, notifies all subscribers.

**Recovery:** `Recover()` returns `nil, nil` — local processes don't survive daemon restarts.

**Cleanup:** No-op (`return nil`).

---

### 2.2 `LocalSidecarRuntime` — CLI flag `"local"` (default)

**Source:** `pkg/runtime/local_sidecar.go`

**Struct:**

```go
type LocalSidecarRuntime struct {
    SidecarBin string
}
```

- `SidecarBin`: Path to sidecar binary. Defaults to `"agentruntime-sidecar"` (resolved via `exec.LookPath`).

**`Name()`** returns `"local"`.

**Spawn sequence:**

1. Validate `cfg.Cmd` non-empty.
2. `findFreePort()` — binds `:0`, closes, returns port. Fallback: `10000 + rand.Intn(55000)`.
3. Marshal `[]string{cfg.Cmd[0]}` as JSON → `AGENT_CMD` env var.
4. Build sidecar command: `exec.CommandContext(ctx, r.sidecarBinary())`.
5. Sidecar environment: `os.Environ()` + `AGENT_CMD=<json>` + `SIDECAR_PORT=<port>` + optionally `AGENT_PROMPT=<prompt>`.
6. Sidecar stdout/stderr → `os.Stderr` (daemon's stderr, for logging).
7. `sidecar.Start()`.
8. **Health check loop:** Poll `http://localhost:<port>/health` every 200ms for up to 15s.
   - Parse response as `{status, agent_type, error_detail}`.
   - `status == "error"` → kill sidecar, return error with `error_detail`.
   - `agent_type != ""` → healthy, proceed.
   - Timeout → kill sidecar, return error.
9. `dialSidecar("local-sidecar-<pid>", "<port>", 0, "")` — prompt not sent via WS (already in `AGENT_PROMPT` env).
10. Override `handle.killFn = func() error { sidecar.Process.Kill() }`.
11. Return `*wsHandle`.

**Key differences from `LocalRuntime`:**

| Aspect | `LocalRuntime` (pipe) | `LocalSidecarRuntime` |
|--------|----------------------|----------------------|
| Output format | Raw process stdout/stderr | Structured NDJSON events via sidecar WS |
| Handle type | `*localHandle` | `*wsHandle` (`SteerableHandle`) |
| Stderr | Separate pipe | `nil` (merged into structured events) |
| Prompt delivery | Write to stdin | `AGENT_PROMPT` env var |
| Steerable commands | Not supported | `SendPrompt`, `SendInterrupt`, `SendSteer`, `SendContext`, `SendMention` |
| Process architecture | Direct: daemon → agent | Two processes: daemon → sidecar → agent |
| Health check | None | HTTP `/health` with 15s timeout |

**Recovery:** `Recover()` returns `nil, nil` — local sidecars don't survive restarts.

**Cleanup:** No-op.

---

### 2.3 `DockerRuntime` — CLI flag `"docker"`

**Source:** `pkg/runtime/docker.go`

**Struct:**

```go
type DockerRuntime struct {
    cfg            DockerConfig
    materializer   dockerMaterializer
    networkManager *NetworkManager
}
```

**`DockerConfig`:**

```go
type DockerConfig struct {
    Image     string     // Default: "agentruntime-agent:latest"
    Network   string     // Docker network name
    DataDir   string     // Persistent data dir for session homes
    ExtraArgs []string   // Additional docker run args
}
```

**Constants:**

```go
const DefaultDockerImage        = "agentruntime-agent:latest"
const dockerSidecarContainerPort = "9090"
const dockerSidecarHealthPath    = "/health"
const dockerSidecarHealthTimeout = 15 * time.Second
const dockerSidecarHealthPoll    = 200 * time.Millisecond
```

**Label keys:**

```go
const dockerTaskLabelKey    = "agentruntime.task_id"
const dockerSessionLabelKey = "agentruntime.session_id"
```

**`Name()`** returns `"docker"`.

**Spawn sequence:**

1. Validate `cfg.Cmd` non-empty.
2. `r.manager().EnsureNetwork(ctx)` — create bridge network.
3. `r.manager().EnsureProxy(ctx)` — start Squid proxy container.
4. `r.prepareRun(cfg)` → `dockerRunSpec{args, cleanup}`:

   a. **Image resolution:** `cfg.Request.Container.Image` → fallback → `DockerConfig.Image` → default `"agentruntime-agent:latest"`.

   b. **Materialization:** If `Request` has `.Claude` or `.Codex` config, calls `materializer.Materialize(req, sessionID)` → produces mounts, cleanup function, and writes `SessionDir`.

   c. **Mounts:** From `cfg.Request.EffectiveMounts()` or fallback `{WorkDir → /workspace, rw}`.

   d. **Environment:** `Request.Env` + `NetworkManager.ProxyEnv()` + `AGENT_CMD=<json>`. Written to temp file via `writeDockerEnvFile()` — **clean-room model, no parent env inheritance**.

   e. **Exact `docker run` flags:**

   ```
   docker run --rm -d
     -p 0:9090
     --init
     --cap-drop ALL
     --cap-add DAC_OVERRIDE
     --security-opt no-new-privileges:true
     --label agentruntime.task_id=<taskID>
     --label agentruntime.session_id=<sessionID>
     --name agentruntime-<sessionID[:8]>
     --workdir /workspace
     --env-file <tmpfile>
     [-t]                                    # if cfg.PTY || req.PTY
     [-v host:container:mode ...]            # mounts
     [--memory <X>]                          # from req.Container.Memory
     [--cpus <X>]                            # from req.Container.CPUs
     [--security-opt <opt> ...]              # from req.Container.SecurityOpt
     [--network agentruntime-agents]
     [ExtraArgs...]
     <image>
   ```

5. Run `docker run`, capture container ID from stdout.
6. `dockerContainerPort(ctx, containerID, "9090")` → resolve host port.
7. `waitForDockerSidecarHealth(ctx, hostPort)` — poll `/health` every 200ms, 15s timeout.
   - Same health response parsing as local sidecar: checks `status`, `agent_type`, `error_detail`.
8. `dialSidecar(containerID, hostPort, 0, dockerPrompt(cfg))`.
   - `dockerPrompt(cfg)`: returns `cfg.Prompt`, or last element of `cfg.Cmd` if `len(cfg.Cmd) > 1`, or `""`.
9. `handle.setCleanup(spec.cleanup)` — registers env file removal and materialization cleanup.
10. Return `*wsHandle`.

**ProcessHandle:** `*wsHandle` (`SteerableHandle`).

**Recovery (`Recover()`):**

1. `docker ps -q --filter label=agentruntime.session_id` — find running containers.
2. For each container:
   a. `docker inspect --format {{json .Config.Labels}} <id>` → extract `agentruntime.session_id` and `agentruntime.task_id`.
   b. Try `dockerContainerPort(ctx, id, "9090")` + `dialSidecar(id, port, 0, "")`.
   c. **WS dial succeeds:** Returns `*wsHandle` with `RecoveryInfo{SessionID, TaskID}`.
   d. **WS dial fails:** Falls back to `newRecoveredDockerHandle(ctx, id, sessionID, taskID)`.

**Cleanup:** Delegates to `r.manager().Cleanup(ctx)`.

---

## 3. ProcessHandle Implementations Summary

### 3.1 `localHandle` (pipe-based)

| Aspect | Detail |
|--------|--------|
| Source | `pkg/runtime/local.go` |
| Used by | `LocalRuntime` |
| `Stdin()` | `io.WriteCloser` — OS pipe to process |
| `Stdout()` | `io.ReadCloser` — OS pipe from process |
| `Stderr()` | `io.ReadCloser` — OS pipe from process (non-nil) |
| `Wait()` | Fan-out pattern. Each caller gets a dedicated channel. |
| `Kill()` | `killLocalProcessGroup(cmd)` — SIGKILL to process group (Unix) or `Process.Kill()` (non-Unix) |
| `PID()` | `cmd.Process.Pid` |
| `RecoveryInfo()` | `nil` |
| Implements `SteerableHandle` | **No** |

### 3.2 `wsHandle` (WebSocket-based)

| Aspect | Detail |
|--------|--------|
| Source | `pkg/runtime/wshandle.go` |
| Used by | `LocalSidecarRuntime`, `DockerRuntime` (primary path) |
| `Stdin()` | `*io.PipeWriter` — write side of internal pipe. Read side relays as `{"type":"prompt","data":{"content":"..."}}` WS frames. |
| `Stdout()` | `*io.PipeReader` — read side of internal pipe. Write side receives NDJSON from WS read goroutine. |
| `Stderr()` | Always `nil` |
| `Wait()` | Returns `done` channel (buffered, size 1) |
| `Kill()` | If `killFn` set (local sidecar): `killFn()`. Else (Docker): `docker stop` + `docker rm`. Always: cancel context, close pipes, close WS, run cleanup. |
| `PID()` | Always `0` |
| `RecoveryInfo()` | Returns `recovery` field (set during Docker recovery) |
| Implements `SteerableHandle` | **Yes** |

### 3.3 `dockerHandle` (docker subprocess wrapper)

| Aspect | Detail |
|--------|--------|
| Source | `pkg/runtime/docker.go` |
| Used by | Not currently used for sidecar spawns (exists for potential non-sidecar docker usage) |
| `Stdin()` | `io.WriteCloser` — pipe to docker CLI stdin |
| `Stdout()` | `io.ReadCloser` — pipe from docker CLI stdout |
| `Stderr()` | `io.ReadCloser` — pipe from docker CLI stderr |
| `Kill()` | `cmd.Process.Kill()` (kills docker CLI process) |
| `RecoveryInfo()` | `nil` |
| Implements `SteerableHandle` | **No** |

### 3.4 `recoveredDockerHandle` (recovery fallback)

| Aspect | Detail |
|--------|--------|
| Source | `pkg/runtime/docker.go` |
| Used by | `DockerRuntime.Recover()` when WS dial fails |
| Construction | `docker logs --follow --since=0 <containerID>` |
| `Stdin()` | `nil` (no input to recovered containers) |
| `Stdout()` | Pipe from `docker logs` stdout |
| `Stderr()` | Pipe from `docker logs` stderr |
| `Kill()` | `docker kill <containerID>` (mutex-guarded) |
| `RecoveryInfo()` | `{SessionID, TaskID}` from container labels |
| Implements `SteerableHandle` | **No** |

---

## 4. WsHandle Deep Dive (`pkg/runtime/wshandle.go`)

### What it is

`wsHandle` is the ProcessHandle + SteerableHandle that communicates with a sidecar binary over WebSocket. It is the **primary handle type** for both `LocalSidecarRuntime` and `DockerRuntime`.

### Construction

`dialSidecar(containerID, hostPort string, sinceOffset int64, prompt string) (*wsHandle, error)`

1. Dials `ws://localhost:<hostPort>/ws?since=<sinceOffset>`.
2. `newWSHandle(conn, containerID, hostPort)` — spawns three goroutines.
3. Sends initial prompt via `handle.SendPrompt(prompt)` if non-empty.

### Internal goroutines

**Read goroutine** — reads `wsServerFrame` from WS:

```go
type wsServerFrame struct {
    Type     string          `json:"type"`
    Data     json.RawMessage `json:"data,omitempty"`
    ExitCode *int            `json:"exit_code,omitempty"`
}
```

| Frame `Type` | Action |
|-------------|--------|
| `"stdout"`, `"replay"` | If data unmarshals as string: write raw bytes to stdoutW. Otherwise: JSON-marshal frame + `\n` to stdoutW. |
| `"agent_message"`, `"tool_use"`, `"tool_result"`, `"result"`, `"progress"`, `"system"`, `"error"` | JSON-marshal frame + `\n` to stdoutW. `"error"` frames also cached in `lastError`. |
| `"exit"` | Extract `exit_code` (default 0) and `error_detail` from data. Send `ExitResult` to `done`. |
| `"connected"`, `"pong"` | Ignored (no-op). |

**Write goroutine (stdin relay):**
- Reads from `stdinR` (pipe read end).
- Trims trailing `\n\r`.
- Sends as `{"type":"prompt","data":{"content":"..."}}`.

**Ping goroutine:**
- Sends WebSocket ping every 30 seconds (`wsHandlePingInterval`).

### Steerable commands (WS client frames)

```go
type wsClientFrame struct {
    Type string `json:"type"`
    Data any    `json:"data,omitempty"`
}
```

| Method | Frame `Type` | Frame `Data` |
|--------|-------------|-------------|
| `SendPrompt(content)` | `"prompt"` | `{"content": "..."}` |
| `SendInterrupt()` | `"interrupt"` | (omitted) |
| `SendSteer(content)` | `"steer"` | `{"content": "..."}` |
| `SendContext(text, filePath)` | `"context"` | `{"text": "...", "file_path": "..."}` |
| `SendMention(filePath, lineStart, lineEnd)` | `"mention"` | `{"file_path": "...", "line_start": N, "line_end": N}` |

### Error handling

- `wsDisconnectError(err)`: If the handle has `RecoveryInfo` and the error is EOF/ErrClosed/WebSocket close codes (Normal, GoingAway, NoStatus, Abnormal), treats it as a clean exit (returns `nil`). Otherwise enriches error with `lastError` if available.
- `lastError`: Populated by `rememberFrameError()` when an `"error"` type frame is received. Used to provide context when the WS disconnects unexpectedly.

### Cleanup lifecycle

- `setCleanup(fn)`: Registers a cleanup function (e.g., env file removal, materialization cleanup).
- `runCleanup()`: Called by `finish()` when process exits, or by `Kill()`. Guarded by `cleanupDone` flag.
- If cleanup is set after the handle has already finished, it runs immediately.

---

## 5. Platform-Specific Process Group Handling

### Unix (`pkg/runtime/local_unix.go`)

```go
func configureLocalProcessGroup(cmd *exec.Cmd) {
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killLocalProcessGroup(cmd *exec.Cmd) error {
    // Negative PID = entire process group
    syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
```

Tolerates `os.ErrProcessDone` and `syscall.ESRCH` (no such process).

### Non-Unix (`pkg/runtime/local_nonunix.go`)

```go
func configureLocalProcessGroup(cmd *exec.Cmd) {}  // no-op
func killLocalProcessGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }
```

---

## 6. Environment Handling (`pkg/runtime/env.go`)

### `buildSpawnEnv(extra map[string]string) ([]string, error)`

Used by `LocalRuntime` only.

- Empty `extra` → returns `nil` (Go's `exec.Cmd` inherits parent env).
- Non-empty `extra` → `os.Environ()` + overlay of extra vars.
- **Reserved keys** (cannot be overridden):
  - `PATH`
  - `LD_PRELOAD`
  - `LD_LIBRARY_PATH`
  - `DYLD_FRAMEWORK_PATH`
  - `DYLD_INSERT_LIBRARIES`
  - `DYLD_LIBRARY_PATH`

### Docker env model (`writeDockerEnvFile`)

**Fundamentally different from local.** Writes ONLY explicit vars to a temp file (`0600` permissions). No parent env inheritance. This is the Docker isolation contract.

Validation:
- Keys: non-empty, no `=`, no whitespace, no NUL.
- Values: no `\r\n`, no NUL.
- Same reserved key blocklist applies.

---

## 7. Network Management (`pkg/runtime/network.go`)

### `NetworkManager`

```go
type NetworkManager struct {
    NetworkName string
    ProxyImage  string
    ensureOnce  sync.Once
    ensureErr   error
}
```

### Constants

| Name | Value |
|------|-------|
| `defaultDockerNetworkName` | `"agentruntime-agents"` |
| `defaultDockerProxyImage` | `"agentruntime-proxy:latest"` |
| `dockerProxyContainerName` | `"agentruntime-proxy"` |
| `dockerProxyPort` | `"3128"` |

### Methods

**`EnsureNetwork(ctx)`:**
- Checks `docker network inspect <name>`.
- If missing: `docker network create <name>`.
- Idempotent — "already exists" is success.

**`EnsureProxy(ctx)`:**
- `sync.Once`-guarded.
- Inspects proxy container. If stopped: remove and recreate.
- Runs: `docker run -d --name agentruntime-proxy --network <name> agentruntime-proxy:latest`.
- "already in use" treated as success (race protection).

**`ProxyEnv()`:**

```go
{
    "HTTP_PROXY":  "http://agentruntime-proxy:3128",
    "HTTPS_PROXY": "http://agentruntime-proxy:3128",
    "NO_PROXY":    "localhost,127.0.0.1,host.docker.internal",
}
```

**`Cleanup(ctx)`:**
1. Stop proxy: `docker stop agentruntime-proxy`.
2. Remove proxy: `docker rm -f agentruntime-proxy`.
3. Remove network: `docker network rm <name>`.
4. Reset `sync.Once` gate.

### Helper functions

| Function | Purpose |
|----------|---------|
| `dockerNetworkExists(ctx, name)` | `docker network inspect` — returns bool |
| `dockerContainerExists(ctx, name)` | `docker inspect --type container` — returns bool, error |
| `dockerInspectRunning(ctx, name)` | `docker inspect --format {{.State.Running}}` |
| `dockerRemoveContainer(ctx, name)` | `docker rm -f` |
| `dockerOutput(ctx, args...)` | Generic `docker <args>` with combined output |
| `dockerObjectMissing(err)` | Checks for "No such container/object/network" strings |

---

## 8. Orphan Recovery Summary

| Runtime | `Recover()` behavior |
|---------|---------------------|
| `LocalRuntime` | Returns `nil, nil`. Local processes don't survive daemon restarts. |
| `LocalSidecarRuntime` | Returns `nil, nil`. Local sidecars don't survive daemon restarts. |
| `DockerRuntime` | Queries `docker ps -q --filter label=agentruntime.session_id`. For each container: extracts labels, tries WS dial → `*wsHandle` with `RecoveryInfo`. Fallback: `docker logs --follow` → `*recoveredDockerHandle`. |

---

## 9. SpawnConfig Field Usage by Runtime

| Field | `LocalRuntime` (pipe) | `LocalSidecarRuntime` | `DockerRuntime` |
|-------|:---------------------:|:---------------------:|:---------------:|
| `SessionID` | — | — | Container name (`agentruntime-<id[:8]>`), labels |
| `AgentName` | — | — | — |
| `Cmd` | Full command executed via `exec.Command` | `Cmd[0]` → `AGENT_CMD` JSON env | `Cmd[0]` → `AGENT_CMD` JSON env |
| `Prompt` | — (caller writes to stdin) | `AGENT_PROMPT` env var | `dockerPrompt(cfg)` → WS prompt or last `Cmd` arg |
| `Env` | Merged onto parent env via `buildSpawnEnv` | — | — (uses `Request.Env` instead) |
| `WorkDir` | `cmd.Dir` | `sidecar.Dir` | Mounted as `/workspace` if no explicit mounts |
| `TaskID` | — | — | Container label `agentruntime.task_id` |
| `Request` | — | — | Mounts, materialization, container config, env |
| `SessionDir` | — | — | Set to materialized session dir path |
| `PTY` | — | — | Adds `-t` flag to `docker run` |

---

## 10. `agentd` Startup Sequence (`cmd/agentd/main.go`)

1. **Dispatch check:** If `os.Args[1] == "dispatch"`, run `runDispatchCommand(os.Args[2:])` and `os.Exit()`.

2. **Parse flags:**
   - `--port` (default `8090`)
   - `--runtime` (default `"local"`)
   - `--data-dir` (default `$AGENTRUNTIME_DATA_DIR` → `$XDG_DATA_HOME/agentruntime` → `~/.local/share/agentruntime`)
   - `--credential-sync` (default `false`)
   - `--max-sessions` (default `0` = unlimited)

3. **Initialize runtime** via `newRuntime(name, dataDir)`:

   | `--runtime` value | Constructor |
   |-------------------|-------------|
   | `"local"` | `runtime.NewLocalSidecarRuntime()` |
   | `"local-pipe"` | `runtime.NewLocalRuntime()` |
   | `"docker"` | `runtime.NewDockerRuntime(DockerConfig{DataDir: dataDir})` |

4. **Initialize session manager:** `session.NewManager()`. Optionally `sessions.SetMaxSessions(n)`.

5. **Recover orphaned sessions:**
   - `rt.Recover(ctx)` → `[]ProcessHandle`.
   - `sessions.Recover(recovered, rt.Name())` → `[]*session.Session`.
   - `restoreRecoveredSessions(logDir, sessions)`:
     - For each session: attempt to load replay buffer from `session.ExistingLogFilePath(logDir, sess.ID)`.
     - `sess.Replay.LoadFromFile(logPath)`.
     - `api.AttachSessionIO(sess, logDir)` — reattach stdio streaming.

6. **Optional credential sync:** `credentials.NewSync(dataDir).Watch(ctx, 30s)`.

7. **Agent registry:** `agent.DefaultRegistry()`.

8. **Start HTTP server:**
   ```go
   srv := api.NewServer(sessions, rt, agents, api.ServerConfig{DataDir, LogDir})
   srv.Start(addr)
   ```

9. **Graceful shutdown** on `SIGINT`/`SIGTERM`:
   - `srv.Shutdown(ctx)` with 5s timeout.
   - `rt.Cleanup(ctx)` — tears down runtime infrastructure (proxy container, network).

---

## 11. Dispatch Subcommand (`cmd/agentd/dispatch.go`)

**Invocation:** `agentd dispatch --config <path> [--server <url>]`

**Purpose:** CLI client that creates a session on a running `agentd` instance, streams NDJSON logs to stdout, and exits with the session's final status code.

**Flags:**
- `--config` (required): Path to YAML session config file.
- `--server` (default `http://localhost:8090`): agentd server URL.

**Flow:**

1. `loadDispatchRequest(configPath)`:
   - Read YAML → `api.SessionRequest`.
   - `expandEnvStrings(&req)` — recursively expands `$VAR` / `${VAR}` in all string fields via `os.ExpandEnv`. Uses `reflect` to walk the entire struct tree including nested slices, maps, and pointers.

2. `client.New(serverURL).Dispatch(ctx, req)` → `{SessionID, WSURL, LogURL}`.

3. Print metadata to stderr:
   ```
   session_id: <id>
   ws_url: <url>
   log_url: <url>
   ```

4. `client.StreamLogs(ctx, sessionID)` → `io.ReadCloser`. `io.Copy(os.Stdout, logs)`.

5. `client.GetSession(ctx, sessionID)` → final status check.

6. **Exit codes:**
   - `"completed"` → exit `0`
   - `"failed"` → exit `1`
   - Other → error message, exit `1`

**Signal handling:** `signal.NotifyContext` for `SIGINT`/`SIGTERM` — cancels the context, which terminates log streaming.

---

## 12. Docker Container Naming and Labeling

| Item | Format |
|------|--------|
| Container name | `agentruntime-<sessionID[:8]>` |
| Task label | `agentruntime.task_id=<taskID>` |
| Session label | `agentruntime.session_id=<sessionID>` |
| Empty values | Default to `"unknown"` in labels, `"unknown"` in container name |

---

## 13. Health Check Protocol (shared by LocalSidecar and Docker)

Both runtimes poll the sidecar's HTTP health endpoint before dialing WS.

**Endpoint:** `GET http://localhost:<port>/health`

**Expected response:**

```json
{
  "status": "ok" | "error",
  "agent_type": "claude" | "codex" | "",
  "error_detail": "..."
}
```

**Logic:**
- `status == "error"` → fatal, abort with `error_detail`.
- `agent_type != ""` → healthy, proceed to WS dial.
- Otherwise → retry.
- **Timeout:** 15 seconds, polling every 200ms.
