# Named Docker Volumes for Session Persistence

## Status

Draft

## Problem

When a Docker-backed session container exits (crash, timeout, or `--rm` cleanup), all session state inside the container is lost. Claude Code stores its conversation history as `.jsonl` files under `/home/agent/.claude/projects/`, and this state is required for `--resume` to work. Today the materializer bind-mounts a host directory for this purpose, but this approach ties persistence to the host filesystem layout and breaks when resuming across different daemon restarts or hosts. Named Docker volumes decouple session state from the host filesystem and survive container removal, enabling true session persistence across container lifecycles.

## Goals

1. Session state (Claude `.jsonl` files) persists across container restarts and removals.
2. A resumed session reuses the same named volume, so Claude Code sees prior conversation history.
3. Volume lifecycle is explicit: volumes are created on session create and removed only on explicit cleanup.
4. The feature is opt-in per session via the API.
5. No breaking changes to existing bind-mount behavior.

## Non-Goals

- Shared volumes across multiple concurrent sessions (1:1 session-to-volume only).
- Volume backup, migration, or replication to remote storage.
- Volume support for the `local` or `local-pipe` runtimes.
- Codex session persistence (Codex does not support `--resume`; can be added later).

---

## Design

### 1. API Surface

#### SessionRequest Changes

Add a `PersistSession` boolean to `SessionRequest` in `pkg/api/schema/types.go`:

```go
type SessionRequest struct {
    // ... existing fields ...
    PersistSession bool `json:"persist_session,omitempty"` // Create a named volume for session state
}
```

When `PersistSession` is `true` and the runtime is `docker`, the daemon creates a named Docker volume and mounts it into the container. When `PersistSession` is `false` (default), behavior is unchanged.

When `ResumeSession` references a prior session that had `PersistSession: true`, the new session automatically inherits `PersistSession: true` and reuses the original session's volume.

#### Session Response Changes

Add a `VolumeName` field to `Session` in `pkg/session/session.go` and the `SessionInfo` response in `pkg/api/schema/types.go`:

```go
// pkg/session/session.go
type Session struct {
    // ... existing fields ...
    VolumeName string `json:"volume_name,omitempty"`
}

// pkg/api/schema/types.go — SessionInfo
type SessionInfo struct {
    // ... existing fields ...
    VolumeName string `json:"volume_name,omitempty"`
}
```

#### Volume Cleanup on DELETE

`DELETE /sessions/:id` gains an optional query parameter:

```
DELETE /sessions/:id?remove_volume=true
```

- `remove_volume=false` (default): session is deleted, volume is preserved for future resume.
- `remove_volume=true`: session is deleted and its named volume is removed.

This keeps the default safe (volumes survive deletion) while giving callers explicit control.

#### Dedicated Volume Pruning Endpoint

A new endpoint for bulk volume cleanup:

```
DELETE /volumes/prune
```

Removes named volumes whose owning session no longer exists in the session manager. Returns a list of pruned volume names and the total reclaimed count.

Response:

```json
{
    "pruned": ["agentruntime-vol-a1b2c3d4", "agentruntime-vol-e5f6g7h8"],
    "count": 2
}
```

### 2. Volume Naming Convention

Volume names follow the pattern:

```
agentruntime-vol-{sessionID[:8]}
```

This mirrors the existing container naming convention (`agentruntime-{sessionID[:8]}` from `dockerContainerName()`). The `vol` infix distinguishes volumes from containers.

A helper function in `pkg/runtime/docker.go`:

```go
func dockerVolumeName(sessionID string) string {
    prefix := sessionID
    if len(prefix) > 8 {
        prefix = prefix[:8]
    }
    return "agentruntime-vol-" + prefix
}
```

Volumes are also labeled for discovery:

```
--label agentruntime.session_id={sessionID}
```

### 3. Volume Lifecycle

#### Creation

Volume creation happens inside `DockerRuntime.prepareRun()`, after materialization and before arg assembly:

```
prepareRun(cfg SpawnConfig)
  1. Resolve image, gather request mounts
  2. Validate mount paths
  3. Materialize agent config (Claude/Codex)
  4. --- NEW: if PersistSession, create named volume ---
     docker volume create --label agentruntime.session_id={sessionID} {volumeName}
  5. Add volume mount to mount list
  6. Build docker run args
```

The volume mount is:

```
-v agentruntime-vol-{id[:8]}:/home/agent/.claude/projects:rw
```

This is the directory where Claude Code writes session `.jsonl` files. The existing bind-mount from the materializer at `/home/agent/.claude` still provides credentials, settings, and `.mcp.json`. The named volume mounts a subdirectory _inside_ that bind-mount's container path, and Docker resolves this correctly: the named volume takes precedence at `/home/agent/.claude/projects` while the bind-mount provides the parent directory contents.

#### Resume

When `ResumeSession` is set and the referenced session has a volume:

1. `handleCreateSession` looks up the original session's `VolumeName`.
2. The new session's `VolumeName` is set to the same value.
3. `prepareRun()` skips `docker volume create` (volume already exists) and mounts the existing volume.
4. Claude Code receives `--resume --session-id {claudeSessionID}` and finds its `.jsonl` files in the volume.

The Claude session ID (needed for `--resume --session-id`) is discovered by one of two mechanisms, tried in order:

1. **Host-side read** (existing path): If the session has a `SessionDir` with a host-side copy, `ReadLastClaudeSessionID()` reads it directly.
2. **Volume-side read** (new path): Run a short-lived `docker run --rm -v {volume}:/data:ro {image} cat` to list `.jsonl` files inside the volume and extract the session ID. This is a fallback for when there is no host-side copy.

In practice, the materializer still creates a host-side session directory even when a named volume is used. The `.jsonl` files are written to the volume (not the host bind-mount), but the materializer's host directory contains credentials and settings. To avoid the `docker run cat` fallback, the sidecar should report the Claude session ID on first use.

**Sidecar session ID reporting**: The sidecar already emits structured events. After Claude starts, the sidecar emits a `system` event:

```json
{
    "type": "system",
    "data": {"claude_session_id": "uuid"},
    "offset": ...,
    "timestamp": ...
}
```

The daemon captures this in `Session.Tags["claude_session_id"]` during event processing. On resume, the daemon reads this tag instead of scanning the filesystem.

#### Container Exit

Containers run with `--rm`, so they are removed on exit. The named volume is **not** affected — Docker volumes survive container removal. No changes to the `--rm` flag are needed.

#### Explicit Removal

Volumes are removed in two ways:

1. **Per-session**: `DELETE /sessions/:id?remove_volume=true` calls `docker volume rm {volumeName}`.
2. **Bulk prune**: `DELETE /volumes/prune` lists all `agentruntime-vol-*` volumes, checks which ones have no corresponding session in the manager, and removes orphans.

#### Daemon Startup Cleanup

On daemon startup, after session recovery, run an orphan volume sweep:

```
docker volume ls --filter label=agentruntime.session_id --format '{{.Name}} {{.Labels}}'
```

For each volume, check if the session ID exists in the recovered session set. Log orphaned volumes but do **not** auto-remove them (data loss risk). The `/volumes/prune` endpoint handles intentional cleanup.

### 4. Mount Pipeline Changes

#### Bypass Validation for Volume Mounts

The `Mount` struct needs a way to distinguish bind-mounts from named volumes. Add a `Type` field:

```go
type Mount struct {
    Host      string `json:"host"      yaml:"host"`
    Container string `json:"container" yaml:"container"`
    Mode      string `json:"mode"      yaml:"mode"`
    Type      string `json:"type"      yaml:"type"` // "bind" (default) | "volume"
}
```

When `Type` is `"volume"`, the `Host` field contains the volume name instead of a host path.

The following functions must check `mount.Type` and skip their logic for volume mounts:

| Function | File | Change |
|----------|------|--------|
| `validateMountPath()` | `pkg/runtime/docker.go` | Skip entirely when `Type == "volume"` |
| `ensureHostMountSource()` | `pkg/runtime/docker.go` | Skip entirely when `Type == "volume"` |

`formatDockerMount()` requires no changes: both bind-mounts and named volumes use the same `-v name_or_path:/container/path:mode` syntax.

#### Mount Ordering

Tests depend on `Mounts[0]` being the `.claude` directory. The volume mount at `/home/agent/.claude/projects` is a child of the `.claude` mount. It must appear **after** the `.claude` mount in the args list so Docker processes it correctly. The implementation appends the volume mount after the materializer mounts.

### 5. Session Directory Interaction

The materializer (`pkg/materialize/materializer.go`) continues to create the host-side session directory under `{dataDir}/claude-sessions/{sessionID}/`. This directory provides:

- `credentials.json` / `.credentials.json` — OAuth credentials (bind-mounted)
- `settings.json`, `.mcp.json` — Claude configuration (bind-mounted)
- `projects/` — **replaced by the named volume when `PersistSession` is true**

When `PersistSession` is true, the materializer still initializes the host-side `projects/` directory (for credentials and settings placement), but the named volume overlays it inside the container. Claude Code writes `.jsonl` files to the volume, not the host.

The `Session.SessionDir` field continues to point to the host-side directory. The `Session.VolumeName` field identifies the volume.

### 6. Recovery

`DockerRuntime.Recover()` lists running containers by label. For volume-aware recovery:

1. After identifying a container, inspect its mounts to detect named volumes matching the `agentruntime-vol-*` pattern.
2. Set `Session.VolumeName` on the recovered session.
3. The recovered session is then resume-capable.

Implementation: use `docker inspect --format '{{json .Mounts}}' {containerID}` to extract volume mounts. Filter for entries where `Type == "volume"` and `Name` starts with `agentruntime-vol-`.

---

## Implementation Plan

### Phase 1: Schema and Types

**Files**: `pkg/api/schema/types.go`, `pkg/session/session.go`

1. Add `PersistSession bool` to `SessionRequest`.
2. Add `Type string` to `Mount` (default `"bind"`).
3. Add `VolumeName string` to `Session`.
4. Add `VolumeName string` to `SessionInfo`.

### Phase 2: Volume Lifecycle in Docker Runtime

**Files**: `pkg/runtime/docker.go`, new `pkg/runtime/volumes.go`

1. Add `dockerVolumeName(sessionID) string` helper.
2. Add `createVolume(ctx, sessionID) (string, error)` — runs `docker volume create` with labels.
3. Add `removeVolume(ctx, volumeName) error` — runs `docker volume rm`.
4. Add `listVolumes(ctx) ([]VolumeInfo, error)` — runs `docker volume ls` with label filter.
5. Add `inspectContainerVolumes(ctx, containerID) ([]string, error)` — extracts volume names from container inspect.

### Phase 3: Mount Pipeline Integration

**Files**: `pkg/runtime/docker.go`

1. Update `validateMountPath()` to skip when `mount.Type == "volume"`.
2. Update `ensureHostMountSource()` to skip when `mount.Type == "volume"`.
3. Update `prepareRun()`:
   - After materialization, if `PersistSession` is true, call `createVolume()`.
   - Append volume mount `{volumeName}:/home/agent/.claude/projects:rw` with `Type: "volume"`.
   - Register volume cleanup in the cleanup chain (only for creation failure; do NOT clean up volume on normal exit).

### Phase 4: Resume Flow

**Files**: `pkg/api/handlers.go`, `pkg/session/session.go`

1. Update `handleCreateSession()`:
   - When `ResumeSession` is set, look up the referenced session's `VolumeName`.
   - If a volume exists, set `PersistSession: true` on the new request and pass the volume name through.
2. Update `lookupResumeSessionID()`:
   - Check `Session.Tags["claude_session_id"]` first.
   - Fall back to existing `ClaudeResumeArgs()` host-side read.
   - Final fallback: volume-side read via `docker run`.
3. Store `claude_session_id` tag during event stream processing when the sidecar reports it.

### Phase 5: Cleanup Endpoints

**Files**: `pkg/api/handlers.go`, `pkg/api/routes.go`

1. Update `handleDeleteSession()`:
   - Parse `remove_volume` query param.
   - If true and session has a `VolumeName`, call `removeVolume()`.
2. Add `handlePruneVolumes()`:
   - List all labeled volumes.
   - Cross-reference with active sessions.
   - Remove orphans.
   - Return pruned list.
3. Register `DELETE /volumes/prune` route.

### Phase 6: Recovery

**Files**: `pkg/runtime/docker.go`

1. Update `Recover()` to inspect container mounts for named volumes.
2. Populate `RecoveryInfo` with volume name (add field to `RecoveryInfo`).
3. Daemon recovery loop sets `Session.VolumeName` from `RecoveryInfo`.

### Phase 7: Daemon Startup Sweep

**Files**: `cmd/agentd/main.go`

1. After recovery, call `listVolumes()` to enumerate orphaned volumes.
2. Log orphan count and volume names at WARN level.
3. Do not auto-remove.

### Phase 8: Sidecar Session ID Reporting

**Files**: `cmd/sidecar/normalize.go`, `cmd/sidecar/ws.go`

1. After Claude starts, emit a `system` event with `claude_session_id`.
2. Extract the session ID from Claude's output or from the filesystem inside the container.

---

## Testing Strategy

### Unit Tests

**`pkg/runtime/docker_test.go`**

| Test | Description |
|------|-------------|
| `TestPrepareRun_PersistSession_CreatesVolume` | When `PersistSession: true`, verify `docker volume create` is called and the volume mount appears in args as `-v agentruntime-vol-{id[:8]}:/home/agent/.claude/projects:rw`. |
| `TestPrepareRun_PersistSession_False_NoVolume` | When `PersistSession: false`, verify no volume mount or `docker volume create` call. |
| `TestPrepareRun_VolumeMount_SkipsValidation` | A mount with `Type: "volume"` does not trigger `validateMountPath()` or `ensureHostMountSource()`. |
| `TestPrepareRun_VolumeMount_Ordering` | Volume mount appears after the materializer's `.claude` mount in the arg list. |
| `TestPrepareRun_Resume_ReusesVolume` | When resuming a session with a volume, the same volume name is mounted without calling `docker volume create`. |
| `TestDockerVolumeName` | Verify naming convention and truncation. |
| `TestRemoveVolume` | Verify `docker volume rm` is called with the correct name. |

**`pkg/api/handlers_test.go`** (or `session_lifecycle_test.go`)

| Test | Description |
|------|-------------|
| `TestCreateSession_PersistSession` | POST with `persist_session: true` sets `VolumeName` on the session. |
| `TestCreateSession_ResumeWithVolume` | POST with `resume_session` referencing a persistent session inherits the volume. |
| `TestDeleteSession_RemoveVolume` | DELETE with `?remove_volume=true` removes the volume. |
| `TestDeleteSession_PreserveVolume` | DELETE without the param preserves the volume. |
| `TestPruneVolumes` | Prune removes orphaned volumes but not active ones. |

**`pkg/runtime/docker_test.go` — Recovery**

| Test | Description |
|------|-------------|
| `TestRecover_WithVolume` | Recovery detects named volumes on recovered containers. |

### Integration Tests

Manual or CI-driven tests with a real Docker daemon:

1. Create a persistent session, send a prompt, verify Claude responds.
2. Delete the session (volume preserved).
3. Create a new session with `resume_session` pointing to the first.
4. Verify Claude's `--resume` works and conversation history is intact.
5. Delete the resumed session with `?remove_volume=true`.
6. Verify the volume is gone (`docker volume ls`).

---

## Data Flow Diagrams

### Session Create with Persistence

```
POST /sessions {persist_session: true, agent: "claude", prompt: "..."}
  |
  v
handleCreateSession()
  |-- validate request
  |-- create Session{VolumeName: "agentruntime-vol-{id[:8]}"}
  |-- prepareSessionDir()  (host-side materializer, as before)
  |
  v
DockerRuntime.Spawn()
  |
  v
prepareRun()
  |-- requestMounts()        -> [{WorkDir:/workspace:rw}]
  |-- validateMountPath()    -> validates bind-mounts only
  |-- materialize()          -> [{.claude:/home/agent/.claude:rw}, ...]
  |-- createVolume()         -> docker volume create agentruntime-vol-{id[:8]}
  |-- append volume mount    -> [{vol:/home/agent/.claude/projects:rw, Type:"volume"}]
  |-- build docker run args
  |
  v
docker run ... -v agentruntime-vol-{id[:8]}:/home/agent/.claude/projects:rw ...
  |
  v
sidecar starts -> Claude runs -> writes .jsonl to /home/agent/.claude/projects/
  |
  v
sidecar emits system event: {claude_session_id: "uuid"}
  |
  v
daemon stores Session.Tags["claude_session_id"] = "uuid"
```

### Session Resume

```
POST /sessions {resume_session: "original-id", agent: "claude", prompt: "continue"}
  |
  v
handleCreateSession()
  |-- lookup original session -> VolumeName: "agentruntime-vol-{id[:8]}"
  |-- set PersistSession: true, VolumeName on new session
  |-- lookup claude_session_id from original Session.Tags
  |
  v
DockerRuntime.Spawn()
  |
  v
prepareRun()
  |-- (volume already exists, skip docker volume create)
  |-- mount existing volume at /home/agent/.claude/projects
  |-- AGENT_CONFIG includes resume_session: "claude-uuid"
  |
  v
docker run ... -v agentruntime-vol-{id[:8]}:/home/agent/.claude/projects:rw ...
  |
  v
Claude --resume --session-id {claude-uuid} -> finds .jsonl in volume -> resumes
```

### Volume Cleanup

```
DELETE /sessions/:id?remove_volume=true
  |
  v
handleDeleteSession()
  |-- sess.Kill()          -> stops container (--rm auto-removes it)
  |-- sess.Replay.Close()
  |-- sess.SetCompleted(-1)
  |-- sessions.Remove()
  |-- docker volume rm agentruntime-vol-{id[:8]}   (remove_volume=true only)
```

---

## Edge Cases and Mitigations

### Volume Orphaning

**Scenario**: Daemon crashes after `docker volume create` but before session registration.

**Mitigation**: On startup, enumerate `agentruntime-vol-*` volumes via `docker volume ls --filter label=agentruntime.session_id`. Cross-reference with recovered sessions. Log orphans at WARN level. The `/volumes/prune` endpoint handles cleanup.

### Concurrent Resume Attempts

**Scenario**: Two `POST /sessions` requests both try to resume the same session simultaneously.

**Mitigation**: The session manager's existing duplicate-session-ID check prevents two sessions with the same ID. For resume, the second request gets a new session ID but both would mount the same volume. This is unsafe. Guard against it: when a resume session's volume is already mounted by a running session, reject the request with `409 Conflict`.

### Volume Name Collision

**Scenario**: Two session IDs share the same 8-character prefix.

**Mitigation**: The 8-character prefix from a UUID has ~4 billion possible values, making collision vanishingly unlikely for reasonable session counts. If collision does occur, `docker volume create` is idempotent for the same name — but the data would be incorrect. To handle this defensively, use a longer prefix or the full session ID. **Decision**: Use the full session ID for volume names to eliminate collision risk:

```
agentruntime-vol-{fullSessionID}
```

Docker volume names support up to 64 characters. A UUID is 36 characters. `agentruntime-vol-` is 18 characters. Total: 54 characters, well within limits. The container name can keep its 8-character prefix since containers are ephemeral.

**Revised naming**:

```go
func dockerVolumeName(sessionID string) string {
    return "agentruntime-vol-" + sessionID
}
```

### Disk Exhaustion

**Scenario**: Accumulated volumes exhaust disk space.

**Mitigation**: Log volume count on daemon startup. The `/volumes/prune` endpoint allows callers to reclaim space. Documentation should recommend periodic pruning. Future work could add a TTL-based auto-prune, but that is out of scope.

### Remote Docker Host

**Scenario**: `DOCKER_HOST` points to a remote daemon (ssh://, tcp://).

**Mitigation**: Named volumes work identically on remote daemons. The `docker volume create/rm/ls` commands respect `DOCKER_HOST` via the existing `dockerCmd()` helper. No special handling needed.

### Volume Mount Overlaying Bind-Mount

**Scenario**: The materializer bind-mounts `/home/agent/.claude`, and the named volume mounts `/home/agent/.claude/projects`. Docker handles this correctly — the volume takes precedence at the subdirectory path.

**Verification**: Add an integration test that writes a file to `/home/agent/.claude/projects/` inside the container and verifies it persists on the volume after container removal.

---

## Security Considerations

1. **Volume isolation**: Each volume is bound 1:1 to a session ID. No cross-session access is possible.
2. **No host path exposure**: Named volumes are managed by Docker's storage driver, not directly accessible from the host filesystem without `docker volume inspect`.
3. **Label-based discovery**: Volumes are labeled with `agentruntime.session_id`, enabling the daemon to enumerate only its own volumes.
4. **Existing security hardening applies**: `--cap-drop ALL`, `--cap-add DAC_OVERRIDE`, `--security-opt no-new-privileges:true` — all apply regardless of volume type.
5. **Credential separation**: Credentials remain in the bind-mounted `.claude` directory (sourced from the materializer). The named volume only holds `.jsonl` conversation history, not secrets.

---

## Configuration

No new daemon flags are required. The feature is controlled per-session via the API's `persist_session` field. The volume naming and label conventions are internal implementation details.

Optional future flags (out of scope):

- `--volume-driver` — Use a non-default Docker volume driver.
- `--volume-ttl` — Auto-prune volumes older than a duration.
