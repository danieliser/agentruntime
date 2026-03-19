# Codebase Analysis: Named Docker Volumes for Session Persistence

## Executive Summary

The agentruntime codebase already has all the foundational pieces for named Docker volume support: a `DockerRuntime` that builds `docker run` args with bind-mounts, a materializer that creates per-session directories, an `agentsessions` package that manages Claude/Codex session state, and a `SessionRequest` schema with `ResumeSession` support. The feature requires wiring a named Docker volume into the existing mount pipeline and ensuring the resume flow works across container lifecycles.

---

## 1. Docker Runtime — Mount Pipeline

### Current Mount Flow

The mount pipeline in `DockerRuntime.prepareRun()` (`pkg/runtime/docker.go:200-336`) follows this sequence:

1. **Request mounts** — `requestMounts(cfg)` resolves `WorkDir` → `/workspace:rw` plus any explicit `SessionRequest.Mounts` (`:219`)
2. **Validation** — each host path validated by `validateMountPath()` (`:221-228`)
3. **Materialization** — if `req.Claude` or `req.Codex` is set, calls `r.materializer.Materialize()` which returns additional mounts (`:230-250`)
4. **Materialized mount validation** — second pass validates materialized mounts (`:242-249`)
5. **Arg building** — mounts formatted as `-v host:container:mode` via `formatDockerMount()` (`:304-311`)

**Key detail**: `ensureHostMountSource()` (`:369-394`) pre-creates host paths before `docker run` to prevent Docker's auto-directory-creation behavior. Named Docker volumes don't have host paths — they use the `volume_name:/container/path` syntax — so this function must be skipped for volume mounts.

### Where Named Volumes Differ from Bind-Mounts

The current `Mount` struct (`pkg/api/schema/types.go:58-62`):

```go
type Mount struct {
    Host      string `json:"host"      yaml:"host"`
    Container string `json:"container" yaml:"container"`
    Mode      string `json:"mode"      yaml:"mode"` // "rw" | "ro"
}
```

Named volumes use the format `-v volume_name:/container/path:rw` — the `Host` field becomes a volume name rather than a host path. The `validateMountPath()` check (`:819-836`) requires absolute paths and checks existence, so named volumes need to bypass this validation.

### Container Naming Convention

Containers are named `agentruntime-{sessionID[:8]}` via `dockerContainerName()` (`:483-489`). Session labels are attached as:
- `agentruntime.task_id` → task ID (`:80`)
- `agentruntime.session_id` → session ID (`:81`)

The volume naming convention should follow the same pattern: `agentruntime-session-{sessionID}` or `agentruntime-session-{sessionID[:8]}`.

### Docker Args Structure

The `--rm` flag is always present (`:288`). This means containers are auto-removed on exit. For session persistence with named volumes, `--rm` is fine — the volume survives container removal. But this is worth noting: the container is ephemeral, only the volume persists.

### Security Hardening Applied to All Containers

Every container gets (`:286-300`):
- `--rm -d --init`
- `--cap-drop ALL --cap-add DAC_OVERRIDE`
- `--security-opt no-new-privileges:true`
- Labels for task/session ID
- `--workdir /workspace`
- `--env-file` (temp file, 0600, cleaned up)

Named volumes inherit no additional security concerns — they're Docker-managed storage that only the container can access.

---

## 2. Session Management — Registry & Lifecycle

### Session Struct (`pkg/session/session.go:27-49`)

```go
type Session struct {
    ID          string                `json:"id"`
    TaskID      string                `json:"task_id,omitempty"`
    AgentName   string                `json:"agent_name"`
    RuntimeName string                `json:"runtime_name"`
    SessionDir  string                `json:"session_dir,omitempty"`
    Tags        map[string]string     `json:"tags,omitempty"`
    State       State                 `json:"state"`
    // ... metrics fields
}
```

`SessionDir` is set during spawn via `cfg.SessionDir` pointer (`pkg/runtime/docker.go:235-237`). The materializer sets this to the host path of the materialized session directory. For named volumes, this field should still be set — but to the volume name or a sentinel value, since there's no host path.

### Session Lifecycle States (`pkg/session/session.go:17-23`)

```
StatePending → StateRunning → StateCompleted | StateFailed
                           → StateOrphaned (via recovery)
```

Session persistence adds a new concern: a session can be "completed" but its volume should persist for future resume. The `Manager.Remove()` call happens on DELETE, but the volume should only be removed on explicit cleanup or pruning.

### Session Recovery (`pkg/runtime/docker.go:507-551`)

`DockerRuntime.Recover()` finds containers with `agentruntime.session_id` labels via `docker ps`. It then either:
1. Connects to the sidecar WebSocket (if sidecar is healthy)
2. Falls back to `docker logs --follow` for output streaming

Recovery doesn't currently handle volumes. The feature should make recovery volume-aware: if a recovered container has an associated named volume, the session should be tagged for resume capability.

---

## 3. Materializer — Per-Session Directory Creation

### Materialize Flow (`pkg/materialize/materializer.go:29-78`)

```
Materialize(req, sessionID, dataDir)
  → materializeClaude(tmpDir, dataDir, sessionID, req, &mounts)
    → claudeMountSource(tmpDir, dataDir, sessionID, req)
      → agentsessions.InitClaudeSessionDir(dataDir, sessionID, projectPath, credentials)
```

The materializer creates session directories under `{dataDir}/claude-sessions/{sessionID}/` with structure:
- `projects/{mangled-project-path}/` — Claude writes `.jsonl` session files here
- `sessions/` — PID-based session discovery index
- `credentials.json` / `.credentials.json` — OAuth credentials

**This is the exact directory that named volumes should replace or supplement.** The `projects/` subdirectory is where Claude Code stores its session state. Currently this is a bind-mount from the host filesystem. A named volume at this path would persist across container restarts without host filesystem involvement.

### Mount Output from Materializer (`pkg/materialize/materializer.go:132-151`)

The materializer appends these mounts:
1. `{claudeDir} → /home/agent/.claude:rw` — the session dir (Mounts[0], tests depend on this ordering)
2. `{claudeStatePath} → /home/agent/.claude.json:rw` — onboarding/trust state
3. Optional: `{memoryPath} → /home/agent/.claude/projects/{hash}:ro` — memory mount

The named volume should mount at `/home/agent/.claude/projects` (Claude's project cache path) or a subdirectory of it. This is where Claude writes session `.jsonl` files that enable `--resume`.

### DataDir vs TmpDir Modes

When `dataDir` is empty, the materializer uses a temp dir that gets cleaned up after the session. When `dataDir` is set (the normal daemon mode), session dirs persist under `{dataDir}/claude-sessions/{sessionID}/`.

Named volumes would replace the host-filesystem-backed persistence with Docker-managed persistence. The materializer should detect when a named volume is requested and skip creating the `projects/` subdirectory on the host (since the volume provides it).

---

## 4. Agent Session Directories — Resume Support

### InitClaudeSessionDir (`pkg/session/agentsessions/claude.go:42-83`)

Creates the session dir structure and copies credentials. Returns the path to mount as `/home/agent/.claude`.

### ReadLastClaudeSessionID (`pkg/session/agentsessions/claude.go:91-138`)

Discovers the most recent Claude session ID from:
1. `sessions/*.json` — PID-based index (sorted by `startedAt`)
2. Fallback: scan `projects/{hash}/*.jsonl` by mtime

This function is called by `ClaudeResumeArgs()` to build `--resume --session-id {id}` flags.

### ClaudeResumeArgs (`pkg/session/agentsessions/claude.go:142-152`)

```go
func ClaudeResumeArgs(dataDir, agentRuntimeSessionID string) ([]string, error) {
    sessionDir := filepath.Join(dataDir, "claude-sessions", agentRuntimeSessionID)
    sessionID, err := ReadLastClaudeSessionID(sessionDir)
    // ...
    return []string{"--resume", "--session-id", sessionID}, nil
}
```

**Critical for the feature**: This function reads from the host filesystem. With named volumes, the session `.jsonl` files live inside the Docker volume, not on the host. The resume flow needs one of:
1. Read the session ID from inside the container (sidecar-mediated)
2. Store the Claude session ID in the agentruntime session metadata (DB or struct) when first created
3. Mount the volume and read it on the host before spawning

### Resume Wiring in API Handler (`pkg/api/handlers.go:104-108`)

```go
resumeSessionID, err := s.lookupResumeSessionID(req.Agent, req.ResumeSession)
```

`lookupResumeSessionID()` (`:416-439`) calls `agentsessions.ClaudeResumeArgs()` with the agentruntime session ID. The result feeds into `agent.AgentConfig.ResumeSessionID`.

### Resume in Agent Command Builder (`pkg/agent/claude.go:32-38`)

```go
resumeSessionID := cfg.ResumeSessionID
if resumeSessionID == "" {
    resumeSessionID = cfg.SessionID
}
if resumeSessionID != "" {
    cmd = append(cmd, "--resume", "--session-id", resumeSessionID)
}
```

If `ResumeSessionID` is empty but `SessionID` is set, it uses `SessionID` as the resume target. This means the first spawn always attempts resume with its own session ID — which correctly becomes a no-op if there's no prior session data.

### Resume in Sidecar AgentConfig (`pkg/runtime/agentconfig.go:28-30`)

```go
if cfg.Request.ResumeSession != "" {
    ac.ResumeSession = cfg.Request.ResumeSession
    hasContent = true
}
```

The `ResumeSession` field from `SessionRequest` is forwarded to the sidecar via `AGENT_CONFIG` env var. The sidecar then passes `--resume --session-id {id}` to Claude Code.

---

## 5. API Schema — SessionRequest

### Relevant Fields (`pkg/api/schema/types.go:15-55`)

```go
type SessionRequest struct {
    SessionID     string  `json:"session_id,omitempty"`     // caller-defined UUID
    ResumeSession string  `json:"resume_session,omitempty"` // session ID to resume
    Mounts        []Mount `json:"mounts,omitempty"`         // explicit mounts
    Container     *ContainerConfig `json:"container,omitempty"` // image, resources, network
    // ...
}
```

The feature needs either:
- A new field (e.g., `PersistSession bool` or `SessionVolume string`) on `SessionRequest`
- Or automatic volume creation when `ResumeSession` matches a prior agentruntime session ID

### ContainerConfig (`pkg/api/schema/types.go:109-115`)

```go
type ContainerConfig struct {
    Image       string   `json:"image,omitempty"`
    Memory      string   `json:"memory,omitempty"`
    CPUs        float64  `json:"cpus,omitempty"`
    Network     string   `json:"network,omitempty"`
    SecurityOpt []string `json:"security_opt,omitempty"`
}
```

Volume configuration could be added here or as a top-level `SessionRequest` field.

---

## 6. API Routes & Handler Patterns

### Route Structure (`pkg/api/routes.go`)

```
GET    /health
POST   /sessions           → handleCreateSession
GET    /sessions            → handleListSessions
GET    /sessions/:id        → handleGetSession
GET    /sessions/:id/info   → handleGetSessionInfo
GET    /sessions/:id/logs   → handleGetLogs
GET    /sessions/:id/log    → handleGetLogFile
DELETE /sessions/:id        → handleDeleteSession
GET    /ws/sessions/:id     → handleSessionWS
```

New endpoints potentially needed:
- Volume cleanup could be handled in `handleDeleteSession` (remove volume on delete)
- Or a separate `DELETE /sessions/:id/volume` for explicit volume cleanup

### Error Response Pattern

All handlers use `gin.H{"error": "message"}` for errors. Status codes follow REST conventions:
- 400 for validation errors
- 404 for missing resources
- 409 for conflicts (duplicate session ID)
- 503 for capacity limits

### Session Info Response (`pkg/api/schema/types.go:140-159`)

The `SessionInfo` struct includes `SessionDir` — this should be updated to include volume information (e.g., volume name, whether a persistent volume is attached).

---

## 7. Server Architecture

### Server Struct (`pkg/api/server.go:19-28`)

```go
type Server struct {
    router   *gin.Engine
    sessions *session.Manager
    runtimes map[string]runtime.Runtime
    runtime  runtime.Runtime    // default runtime
    agents   *agent.Registry
    dataDir  string
    logDir   string
    srv      *http.Server
}
```

The `dataDir` is used for session directories. Named volume management would either:
1. Use Docker CLI commands (`docker volume create/rm`) via `DockerRuntime`
2. Or manage volume lifecycle in the API handler layer

### prepareSessionDir (`pkg/api/handlers.go:459-487`)

This function is called before spawn to set up the session directory. It calls `agentsessions.InitClaudeSessionDir()` and sets `sess.SessionDir`. This is where named volume creation should be triggered.

---

## 8. Test Patterns

### Docker Runtime Tests (`pkg/runtime/docker_test.go`)

Tests use `installFakeDocker(t, script)` to install a shell script that mocks Docker CLI commands. Each test:
1. Creates a `DockerRuntime` with a test config
2. Calls `prepareRun()` to get the `dockerRunSpec` (args + cleanup)
3. Asserts specific flags are present in the args
4. Calls `spec.cleanup()` in defer

**Pattern for volume tests**: Assert that `-v agentruntime-session-{id}:/home/agent/.claude/projects:rw` is present in the generated args.

### Materializer Tests (`pkg/materialize/materializer_test.go`)

Tests use `mustMaterializeWithDataDir()` helper. Key patterns:
- `findMount(t, result.Mounts, containerPath)` — finds a mount by container path
- `hasMount(result.Mounts, containerPath)` — checks if a mount exists
- `readJSONFile(t, path, &out)` — reads and unmarshals JSON
- Tests verify both mount creation and cleanup behavior

**Pattern for volume tests**: Verify that when volume mode is requested, the materializer returns a volume mount instead of a bind-mount, and that `CleanupFn` does NOT delete the volume (it should persist).

### Session Tests (`pkg/session/agentsessions/claude_test.go`)

Pure filesystem tests — create dirs, write files, verify reads. No mocking needed.

### API Lifecycle Tests (`pkg/api/session_lifecycle_test.go`)

Integration-style tests using `httptest.Server`. Pattern:
- `newTestServer(t)` / `newTestServerWithMaxSessions(t, max)` — creates server with fake runtime
- `mustCreateSession(t, ts, req)` — POST /sessions
- `deleteSession(t, ts, id)` — DELETE /sessions/:id
- Stress tests for concurrent operations, goroutine leak detection

---

## 9. Knowledge from Persistence Project (Prior Art)

The `docs/PERSISTENCE-KNOWLEDGE-EXTRACTION.md` documents the battle-tested pattern from the PAOP/Persistence executor layer (Section 10):

```python
# Volume name convention
volume_name = f"paop-session-{session_id}"

# Mount at Claude's project cache
"--volume", f"{volume_name}:/home/appuser/.claude/projects"

# Create volume explicitly
docker volume create {volume_name}

# Remove on deregistration
docker volume rm {volume_name}
```

Key insights from persistence:
- **Persistent containers** omit `--rm` and stay alive between commands
- **Named volumes** survive container removal — session `.jsonl` files persist
- **Volume cleanup** happens on deregistration (explicit delete), not on container exit
- **Container user**: persistence used `/home/appuser`, agentruntime uses `/home/agent` — the mount target path differs

---

## 10. File Organization Recommendations

Based on existing patterns, new code should live in:

| Component | Location | Rationale |
|-----------|----------|-----------|
| Volume lifecycle (create/remove) | `pkg/runtime/docker.go` or new `pkg/runtime/volumes.go` | Docker CLI operations belong in the runtime package |
| Mount type extension | `pkg/api/schema/types.go` | Schema changes for `Mount` or `SessionRequest` |
| Materializer volume support | `pkg/materialize/materializer.go` | Extend `materializeClaude()` to handle volume mounts |
| Session volume metadata | `pkg/session/session.go` | Add volume name field to `Session` struct |
| Resume-from-volume logic | `pkg/session/agentsessions/claude.go` | Extend `ClaudeResumeArgs()` for volume-backed sessions |
| API handler volume management | `pkg/api/handlers.go` | Volume create on session create, volume delete on session delete |
| Tests | Alongside each modified file (`*_test.go`) | Follow existing patterns |

---

## 11. Integration Points Summary

### Data Flow for Session Create with Volume

```
POST /sessions {resume_session: "prev-session-id"}
  → handleCreateSession()
    → prepareSessionDir() — create session dir + named volume
    → DockerRuntime.Spawn()
      → prepareRun()
        → materializer.Materialize() — returns bind-mounts + volume mount
        → build docker run args with -v volume_name:/path:rw
    → AttachSessionIO() — tee stdout to replay buffer + log file
```

### Data Flow for Resume

```
POST /sessions {resume_session: "prev-session-id"}
  → lookupResumeSessionID()
    → ClaudeResumeArgs(dataDir, "prev-session-id")
      → ReadLastClaudeSessionID() — reads from session dir (host-side)
      → returns ["--resume", "--session-id", "claude-session-uuid"]
  → agent.BuildCmd() adds --resume --session-id flags
  → DockerRuntime.Spawn() — mounts same named volume
```

### Data Flow for Cleanup

```
DELETE /sessions/:id
  → handleDeleteSession()
    → sess.Kill() — stops container
    → sess.Replay.Close()
    → sess.SetCompleted(-1)
    → sessions.Remove()
    → docker volume rm agentruntime-session-{id} (if explicit cleanup requested)
```

---

## 12. Risks & Edge Cases

1. **Volume orphaning**: If the daemon crashes between volume creation and session registration, the volume is orphaned. Need a cleanup sweep on daemon startup.

2. **Concurrent access**: Two sessions cannot safely share a volume. The `Session.ID` → volume name mapping ensures 1:1 binding.

3. **Disk usage**: Named volumes accumulate without bound unless pruned. The existing `PruneOldSessions()` (`pkg/session/agentsessions/codex.go:85-120`) should be extended to prune volumes.

4. **Resume ID discovery**: Claude session IDs live inside the volume, not on the host. Either the sidecar must report the session ID after first run, or agentruntime must track it in the `Session` struct/metadata.

5. **Mount ordering**: Tests depend on `Mounts[0]` being the `.claude` dir mount (`materializer.go:131`). Volume mounts must maintain this ordering contract.

6. **validateMountPath bypass**: Named volumes don't have host paths. The validation in `docker.go:221-228` and `:242-249` must skip volume mounts (no `filepath.IsAbs` check, no `os.Stat`).

7. **ensureHostMountSource bypass**: Similarly, `ensureHostMountSource()` at `:309` must not attempt to create a host-side file/directory for volume mounts.
