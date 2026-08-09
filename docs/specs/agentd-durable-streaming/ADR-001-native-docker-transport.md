# ADR-001: Direct native JSON over durable Docker stdio

Status: **PROPOSED FOR GATE G0 APPROVAL**

Date: 2026-08-09

Task IDs: STR-001, STR-002, STR-003, STR-004

## Decision

Run Claude and Codex directly as the container workload and keep their native
line-oriented JSON as the canonical transport. Remove the sidecar WebSocket,
normalization, byte-offset replay, health-port, and public event-identity roles.

AgentD will use Docker's native primitives:

- `docker run -d -i` with `OpenStdin=true` and `StdinOnce=false`;
- `docker attach --sig-proxy=false` for native JSON input;
- one `docker logs --follow --since=0 --timestamps` stream for retained history
  followed by live stdout/stderr;
- `docker wait` and `docker inspect` for terminal state and reconstruction.

No replacement in-container transport owner is required by the G0 evidence.

## Evidence

The opt-in integration test
`TestDockerDirectAttach_ReconnectsStdinAndRecoversOrderedOutput` proves against
a real Docker 29.4.0 daemon that:

1. a detached `-i` container accepts a native JSON line;
2. terminating the first attach client does not terminate the container or
   close container stdin;
3. a second attach client can send another native JSON line;
4. Docker retains both stdout records in order from container start;
5. stderr remains a separate log stream; and
6. retained records carry stable RFC3339Nano Docker timestamps.

Run it with:

```sh
AGENTRUNTIME_DOCKER_INTEGRATION=1 go test ./pkg/runtime \
  -run TestDockerDirectAttach_ReconnectsStdinAndRecoversOrderedOutput \
  -count=1 -v
```

The deterministic contract fixtures under `testdata/native-streams/` cover the
Claude stream-json and Codex app-server input/output shapes without invoking a
paid provider session.

## Recovery algorithm

The event database, not Docker logs, is the public replay authority. Docker
logs are the recovery journal for the interval around an AgentD failure.

For each container generation:

1. open `docker logs --follow --since=0 --timestamps` once so retained and live
   records arrive on one connection;
2. split stdout and stderr without treating stderr as provider JSON;
3. strip and parse Docker's timestamp prefix while preserving the exact native
   record bytes;
4. compare retained records, in stream order, with the generation's stored raw
   record hashes;
5. skip the exact committed prefix;
6. append unseen records through the normal atomic sequence allocator; and
7. publish only after the event transaction commits.

This prefix comparison is positional. Repeated identical provider records are
therefore not collapsed as duplicates.

If Docker history is shorter than, diverges from, or begins after the stored
prefix, AgentD records `indeterminate`. It must not guess a boundary, silently
skip output, or restart the paid provider process.

## Input recovery rule

AgentD persists a control/input command before sending its single native JSON
line. Reconnecting an input attach does not automatically resend an
unacknowledged prompt, steer, interrupt, or approval response. The provider
adapter must reconcile an observable acknowledgement or mark the command
outcome `indeterminate`; blind resend could duplicate paid work.

## Why direct attach

- Claude already exposes stream-json stdin/stdout.
- Codex app-server already exposes JSON-RPC over stdio.
- Docker can keep stdin open across attach-client loss.
- Docker logs can supply retained-then-live records from container start.
- AgentD will already own durable event identity and replay, so a second replay
  protocol inside the container adds conflicting authority without improving
  the public contract.

## Constraints

- Containers must use an inspectable Docker logging driver that supports
  `docker logs`; production creation will explicitly select `json-file` unless
  a qualified equivalent is configured.
- Log rotation/truncation cannot be silent. The resolved log driver/options are
  persisted with the runtime generation and checked during reconstruction.
- The Docker log must remain available until AgentD has reconciled the terminal
  receipt and archived all committed events.
- Docker timestamps help merge stdout/stderr after restart, but provider stdout
  remains the semantic stream. Stderr is diagnostic and never parsed as native
  provider JSON.
- A daemon failure during an input write can be ambiguous. Durable command
  state and explicit `indeterminate` handling remain required.

## Removal map after Gate G2

Remove or replace these sidecar-owned semantics only after native parity tests
pass:

- sidecar HTTP health and exposed port 9090;
- sidecar WebSocket command frames;
- sidecar replay buffer and byte offsets;
- sidecar event normalization as the canonical output;
- `wsHandle` as the Docker process handle; and
- recovery preference for a sidecar WebSocket endpoint.

Agent-specific command construction, clean environment construction, approval
responses, and native control messages move behind the canonical native
transport interface. They are behavior to preserve, not sidecar protocol.

## Rejected alternatives

### Keep the current sidecar protocol

Rejected because it creates a second event schema, replay cursor, buffering
authority, and unauthenticated in-container network endpoint.

### Replace it with another persistent in-container broker

Not selected because direct stdin reattachment and Docker retained output were
proven. Reconsider only if qualification exposes a Docker/platform where the
required complete-prefix proof cannot be established.

### Resume from Docker timestamps alone

Rejected because timestamps are not event identities and boundary inclusion can
produce duplicates. Recovery uses full positional prefix comparison; timestamps
are metadata and stdout/stderr merge aids only.

## Consequences

- AgentD becomes the only owner of public event IDs, sequences, replay cursors,
  lifecycle state, and terminal receipts.
- Docker reconstruction reads the complete generation log unless a future
  qualified checkpoint format preserves the same proof.
- Log retention and disk growth become explicit operational concerns rather
  than hidden sidecar ring-buffer truncation.
- Local-process durability remains out of scope; this decision qualifies Docker.
