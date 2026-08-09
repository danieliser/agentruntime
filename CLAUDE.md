# AgentRuntime contributor notes

Read [AGENTS.md](AGENTS.md) first; it is the project contract. The active durable-streaming tracker is [docs/specs/agentd-durable-streaming/TASKS.md](docs/specs/agentd-durable-streaming/TASKS.md).

## What AgentD is

AgentD is the execution runtime for Claude Code and Codex. It owns durable logical sessions, runtime generations, provider-native JSON transport, replay, controls, Docker reconstruction, and terminal receipts. External orchestrators own workflow state and retries.

There is no execution sidecar. Do not add a replacement WebSocket broker, port 9090, mixed byte/event cursor, or second replay authority.

## Canonical paths

```text
cmd/agentd/              daemon + dispatch/attach/chat CLI
pkg/api/                 v1 routes, controls, lifecycle, recovery
pkg/durable/             state/store contracts
pkg/durable/sqlite/      migrations and SQLite implementation
pkg/eventstream/         commit-before-publish event ledger
pkg/nativeprotocol/      Claude/Codex native JSON transport
pkg/runtime/             local and Docker handles
pkg/chat/                persisted named-chat orchestration
```

## Invariants

- Provider raw JSON is immutable and remains canonical.
- Event publish occurs only after the SQLite append commits.
- Public cursors are per-session durable sequences.
- Same idempotency key + same request is lookup; changed request conflicts.
- Reconnect does not create a generation. Explicit resume creates `N+1` only after `lost`.
- Terminal sessions and receipts are immutable.
- Recovery uncertainty becomes `indeterminate`; never guess or blindly resend paid work.
- Docker image digest, labels, sandbox profile, and container identity must agree with durable admission.
- Secrets are launch-only, explicitly granted, and never persisted or logged as values.

## Data root

Default state lives under `~/.agentd`. `AGENTRUNTIME_DATA_DIR` or `--data-dir` relocates database, backups, chat JSON, logs, credentials, and session material together.

## Development loop

Use red-green-refactor. Before claiming completion:

```sh
go test ./...
go test -race ./pkg/api ./pkg/eventstream ./pkg/runtime
go vet ./...
git diff --check
```

For real Docker attach/reconnect qualification:

```sh
AGENTRUNTIME_DOCKER_INTEGRATION=1 \
AGENTRUNTIME_DOCKER_TEST_IMAGE=alpine:3.20 \
go test ./pkg/runtime \
  -run TestDockerDirectAttach_ReconnectsStdinAndRecoversOrderedOutput \
  -count=1 -v
```

## Compatibility warning

Some unversioned daemon routes, the TUI, dashboards, Go client, and legacy chat-log pagination still use byte cursors while migration is in progress. Do not interpret those offsets as v1 event sequences or build new consumers on them.
