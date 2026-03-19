# Spec: Named Docker Volumes for Session Persistence

**Status:** Draft
**Task:** #9
**Effort:** Medium (~150 LOC)
**Source:** persistence `executor/docker_config.py`, `executor/docker_lifecycle.py`

## Problem

Agentruntime creates ephemeral session directories that are destroyed when a
container exits. There's no way to resume a session in a new container — all
Claude project cache data (`.claude/projects/`) is lost. This means session
resumption (`--resume`) only works within a single container lifetime.

## Solution

Use named Docker volumes (`agentruntime-session-{id}`) mounted at the Claude
project cache path. Volumes persist across container restarts, enabling true
session continuity.

## Design

### Volume Naming

```
agentruntime-session-{session_id}
```

Where `session_id` is the UUID assigned by the daemon when creating the session.

### Mount Point

```
/root/.claude/projects    # or /home/appuser/.claude/projects
```

This is where Claude Code stores its project-specific session data. The
`pkg/materialize` package already handles the path hashing for this directory.

### Lifecycle

1. **Create**: On session spawn, `docker volume create agentruntime-session-{id}`
2. **Mount**: Add `--volume agentruntime-session-{id}:/root/.claude/projects` to container args
3. **Reuse**: When resuming a session, mount the existing volume into the new container
4. **Cleanup**: On explicit session delete, `docker volume rm agentruntime-session-{id}`

### Session Types

| Mode | Volume Behavior |
|---|---|
| One-shot (prompt mode) | No volume — ephemeral session, auto-cleanup |
| Interactive | Named volume — persists for future steering/resume |
| Resume | Mount existing volume from original session |

## Implementation

### `pkg/runtime/docker.go`

Add to `SpawnConfig`:
```go
type SpawnConfig struct {
    // ... existing fields ...
    Persistent  bool   // If true, use named volume
    SessionID   string // Used for volume naming
    ResumeFrom  string // If set, mount this session's volume
}
```

Add volume management methods:
```go
func (d *DockerRuntime) createSessionVolume(sessionID string) error
func (d *DockerRuntime) removeSessionVolume(sessionID string) error
func (d *DockerRuntime) sessionVolumeExists(sessionID string) bool
```

### Container args

When `Persistent` is true:
```go
args = append(args,
    "--volume", fmt.Sprintf("agentruntime-session-%s:/root/.claude/projects", sessionID),
)
// Do NOT add --rm flag for persistent containers
```

### Session cleanup hook

In `pkg/session/manager.go`, when a session is explicitly deleted:
```go
if session.Persistent {
    runtime.removeSessionVolume(session.ID)
}
```

### API surface

Add to session create request:
```json
{
  "persistent": true
}
```

Add to session info response:
```json
{
  "volume": "agentruntime-session-abc123",
  "persistent": true
}
```

## Testing

- Create persistent session → volume exists (`docker volume ls`)
- Resume session → same volume mounted
- Delete session → volume removed
- One-shot session → no volume created
- Container crash + restart → volume still exists, data intact
