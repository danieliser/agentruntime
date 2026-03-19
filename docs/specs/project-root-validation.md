# Spec: Project Root Validation

**Status:** Draft
**Task:** #4
**Effort:** Quick win (~80 LOC)
**Source:** persistence `executor/security.py`

## Problem

Agentruntime accepts any working directory for agent sessions with zero validation.
A malicious or misconfigured request could point an agent at `~/.ssh`, `~/.aws`, or
`/` — giving it access to secrets, credentials, or the entire filesystem.

## Solution

Validate the working directory at session spawn time. Reject dangerous paths before
the agent process starts.

## Validation Rules

1. **Must be absolute path** — reject relative paths
2. **Resolve symlinks** — detect traversal via symlink chains
3. **No `..` components** — reject explicit traversal attempts
4. **Not filesystem root** — reject `/`
5. **Not a sensitive directory** — reject paths containing:
   - `.ssh`
   - `.gnupg`
   - `.aws`
   - `.config/gcloud`
   - `.kube`
   - `.docker`
   - `Library/Keychains`
6. **Must exist and be a directory** — reject nonexistent or file paths

## Implementation

### File: `pkg/session/validate.go` (new)

```go
package session

var sensitiveDirs = []string{
    ".ssh", ".gnupg", ".aws", ".config/gcloud",
    ".kube", ".docker", "Library/Keychains",
}

func ValidateWorkDir(path string) error {
    // 1. Must be absolute
    // 2. Resolve symlinks (filepath.EvalSymlinks)
    // 3. Check for ".." in original path
    // 4. Not "/"
    // 5. Check against sensitive dirs (relative to $HOME)
    // 6. Stat — must be a directory
}
```

### Call site

In `pkg/session/manager.go` (or wherever sessions are created), call
`ValidateWorkDir(config.WorkDir)` before spawning the runtime.

For Docker runtime, also validate in `pkg/runtime/docker.go` before
bind-mounting the workspace.

### Error response

Return HTTP 400 with:
```json
{
  "error": "invalid_work_dir",
  "message": "Working directory contains sensitive path: .ssh"
}
```

## Testing

- Reject `~/.ssh`, `/`, `../../../etc/passwd`
- Accept normal project dirs like `/home/user/projects/myapp`
- Symlink resolution test (symlink to .aws should be rejected)
- Docker bind-mount verification (rejected before mount happens)
