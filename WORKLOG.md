# AgentD overnight qualification worklog

## Running status

- Started: 2026-08-20 (America/New_York)
- Branch/head at start: `main` at `bc5b6e3`
- Writable repository: `/Users/danieliser/Toolkit/agentruntime` only
- Read-only qualification source: `/Users/danieliser/Toolkit/trading-floor`
- Pre-existing untracked paths preserved: `.opentraces.json`, `.opentraces/`

## Required reading and baseline

- Read and checked against current code: `docs/reports/AGENT_RUNTIME_AUDIT.md`,
  `docs/slices/027-agentd-v212-qualification.md`,
  `docs/slices/025-agentd-v21-activation.md`, and checklist items M-5/M-6/M-7 in
  Trading Floor.
- Confirmed current release baseline is `v2.1.2` at `d0528ca`; repository head
  contains only a later documentation commit (`bc5b6e3`).
- Confirmed the installed Docker policy network `agentruntime-policy-v1` is
  `internal=true`; the current proxy uses one static wildcard-domain Squid
  configuration rather than the required per-policy exact-host allowlist.
- Confirmed `agentruntime-agent:latest` currently exists locally, but `/health`
  reports only registered runtime names and `DockerRuntime.CheckAdmission` checks
  Docker daemon reachability, not the configured image.
- `go test ./...`: PASS on untouched `bc5b6e3`.
- First `go test -race ./...`: FAIL on untouched `bc5b6e3` in
  `TestNativeDockerHandleUsesAttachLogsAndWait`; fake-Docker stdin capture was
  empty after its two-second deadline. This is a pre-existing timing/race-suite
  gate and no commit will be made while it remains red.

## Phase 1

### 1. Policy-controlled egress

- Complete: commit `f14f88c` (`feat(ACT-1013): enforce exact-host policy egress`).
- Added execution policy `2.0` with a caller-visible, canonical
  `egress_allowlist`. Exact lowercase DNS hosts only; wildcards, suffix rules,
  schemes, ports, IP literals, duplicates, and endpoints outside AgentD's
  advertised provider/tool set fail admission. An empty list is valid and
  denies all egress.
- The canonical allowlist is serialized into the execution policy and therefore
  covered by `execution_policy_hash`; a focused test proves changing only the
  allowlist changes the hash.
- Added per-session, policy-hash-scoped internal Docker networks and
  AgentD-managed Squid proxies. The proxy is the only dual-homed component,
  accepts HTTPS CONNECT only for exact allowed hosts, and has Squid and Docker
  logs disabled. Policy config directories/files are `0700`/`0600`.
- Caller proxy environment variables are rejected. AgentD supplies upper- and
  lowercase HTTP/HTTPS/ALL/NO proxy variables; removing them still cannot bypass
  the internal network.
- Capability metadata now advertises the policy field, default-deny behavior,
  exact-host/proxy requirements, direct-DNS/IP and environment-bypass refusal,
  and the exact provider/tool endpoint sets.
- Real Docker qualification (`AGENTRUNTIME_DOCKER_INTEGRATION=1 go test
  ./pkg/runtime -run TestDockerRestrictedWorkspaceQualification -count=1 -v`):
  PASS. From real restricted containers it proved `api.openai.com` reachable
  through the proxy, `example.com` refused, direct provider and `1.1.1.1`
  connections refused with proxy variables removed, and an empty allowlist
  denied the otherwise supported provider endpoint.
- Verification before commit: `go test ./...` PASS; `go test -race ./...` PASS;
  `git diff --check` PASS.

### 2. Readiness truthfulness

- Complete: commit `95f2b5a` (`fix(ACT-1014): fail readiness without runtime image`).
- `DockerRuntime.CheckAdmission` now verifies both Docker daemon reachability and
  the configured runtime image before admission.
- A new stable `runtime_unavailable` error is returned as HTTP 503 before the
  durable session row or container is created. Idempotent inspection of an
  already-admitted session remains available during later runtime outages.
- `/health` now runs bounded readiness checks for registered runtimes, exposes
  per-runtime `ready|unavailable` state, and returns HTTP 503/status `error` if
  any advertised runtime cannot admit work.
- `install.sh` now refuses installation when Docker or the required
  `agentruntime-agent:latest` image is absent instead of installing a daemon
  that would advertise unusable Docker admission.
- Real Docker qualification (`AGENTRUNTIME_DOCKER_INTEGRATION=1 go test
  ./pkg/api -run TestDockerImageRemovalMakesReadinessAndAdmissionFailClosed
  -count=1 -v`): PASS. The test creates a temporary tag for the runtime image,
  proves readiness green, removes that tag, then proves readiness 503 and typed
  admission refusal with zero durable sessions and no container creation.
- Verification before commit: `go test ./...` PASS; `go test -race ./...` PASS;
  `bash -n install.sh` PASS; `git diff --check` PASS.

### 3. No ambient plugin processes

- Complete: commit `1610aa7` (`test(ACT-1015): prove no ambient policy plugins`),
  verifying the existing policy resolver and clean materialization boundary.
- Added an opt-in real Docker test that resolves the actual restricted Codex
  app-server command, proves `plugins` and `enable_mcp_apps` are disabled,
  starts the real app-server with an empty MCP/plugin policy, and inspects every
  process command line inside the live session container.
- Real Docker qualification (`AGENTRUNTIME_DOCKER_INTEGRATION=1 go test
  ./pkg/api -run TestDockerRestrictedCodexStartsNoAmbientPluginProcesses
  -count=1 -v`): PASS. The Codex app-server was live and zero `codex_apps`,
  `mcp-server`, or `mcp_server` processes existed.
- Verification before commit: `go test ./...` PASS; `go test -race ./...` PASS;
  `git diff --check` PASS.

### 4. Release v2.2.0

- Complete: release commit `530bad2b5dab578589bf422c4573d6f3182f2389`,
  annotated tag `v2.2.0`, and GitHub release
  `https://github.com/danieliser/agentruntime/releases/tag/v2.2.0`.
- Canonical source and Python wrapper metadata are `2.2.0`; release notes list
  the policy-version migration, exact-host proxy enforcement, truthful
  readiness/admission, plugin-process proof, stamp changes, and unchanged
  result/receipt/event/replay/auth contracts.
- Qualified binaries select `agentruntime-agent:2.2.0` and
  `agentruntime-proxy:2.2.0` and require OCI version/revision labels matching
  their embedded build identity. Development builds remain on `:latest` without
  claiming a release stamp.
- Installer and reinstall paths inject exact Go `Version`/`Commit` ldflags and
  verify the binary. The installer also checks both exact Docker tags and OCI
  labels before activation. Release CI now refuses wrapper/tag version drift.
- Added `scripts/verify-release.sh` to build and verify the exact binary, source
  and wrapper versions, optional release tag, and optional Docker labels.
- Pre-release-source verification: `go test ./...` PASS; `go test -race ./...`
  PASS; `go vet ./...` PASS; shell syntax and `git diff --check` PASS.
- Built and inspected exact local `agentruntime-agent:2.2.0` and
  `agentruntime-proxy:2.2.0` images; both OCI version/revision stamps matched the
  release commit. `REQUIRE_RELEASE_TAG=1 VERIFY_DOCKER_IMAGES=1
  ./scripts/verify-release.sh 2.2.0 530bad2b5dab578589bf422c4573d6f3182f2389`
  passed after tagging.
- Retained Trading Floor public contract suites passed read-only: 20 tests in
  `test_agentd_v21_contract.py`, `test_agentd_v212_contract.py`,
  `test_agentd_port.py`, and `test_agentd_source_scout.py`. This was not a live
  candidate canary; Trading Floor still needs the reviewed exact pin/policy
  migration before its live re-run.
- Hosted GitHub CI and trusted PyPI release workflows both completed
  successfully.

## Phase 2

### 5. Per-session resource ceilings

- Complete: commit/release `b4deea2a700f7d997ee94d8d050f961a3bd3965e`
  (`v2.2.1`). Hosted CI and PyPI publishing passed.
- Added compatible execution policy `2.1`; policy `2.0` remains accepted with
  its original implicit fixed limits. Policy `2.1` canonicalizes memory, CPU,
  PID, and open-file ceilings into `effective_policy_sha256`; changing a limit
  changes the hash.
- Defaults/maximums are 2 GiB, 2 CPU cores, 256 PIDs, and 1,024 open files.
  Minimums are 64 MiB, 0.1 CPU, 16 PIDs, and 64 open files. Invalid, non-finite,
  below-minimum, above-maximum, and legacy container resource overrides fail
  before admission with typed `resource_limit_exceeded`.
- Capabilities advertise the policy field, defaults, minimums, maximums, and
  breach code. `docs/execution-policy.md` documents semantics and compatibility.
- Real Docker ceiling qualification: PASS. Docker HostConfig proved 512 MiB,
  0.5 CPU, 64 PIDs, and soft/hard 256 FD limits on the live session container.
- Real Docker breach qualification: PASS. A 256 MiB allocation in a 64 MiB
  session produced Docker `OOMKilled=true`, exit 137, `SIGKILL`, and stable
  `resource_limit_exceeded` terminal proof.
- CPU quota is kernel throttling, not a terminal error. PID and FD exhaustion
  are kernel refusals visible to the provider; AgentD does not infer a terminal
  cause from provider output. This preserves truthful classification rather
  than guessing from an exit code.

### 6. Thirty-session concurrency proof

- Complete: commit/release `5dca0fc736ec2523b9f680a99d036ba3bdee5b7c`
  (`v2.2.2`).
- Replaced the obsolete scenario, which no longer compiled and used removed
  unversioned HTTP/WebSocket routes. The new opt-in process-boundary scenario
  uses bearer-authenticated v1 durable dispatch and receipt APIs with a
  deterministic Claude native-protocol fixture (no model calls).
- The gate now asserts `completed == 30` from both durable session state and
  immutable receipt state. A nonempty output frame, error frame, or socket
  closure cannot count as completion.
- Run command: `go test -tags='e2e concurrency' -timeout=300s ./pkg/e2e -run
  TestConcurrency_30Sessions -count=1 -v`: PASS twice at 30/30.
- Preserved evidence: `.artifacts/concurrency/20260820T084224Z-99552/` with
  private `0700` directories and `0600` `environment.json`, `results.json`, and
  `daemon.log`. The environment artifact contains no ambient environment or
  credentials.
- Second recorded run: latency p50 1.236 s, p95 1.247 s, max 1.249 s; peak
  AgentD RSS 50,048 KiB; peak virtual memory 411,382,832 KiB (macOS virtual
  address-space accounting); peak AgentD open descriptors 136; peak AgentD
  process tree 61 (daemon plus 30 providers and transient descendants).

### 7. Diagnostic log hygiene

- Implementation complete in commit `d537d2b`; release `v2.2.3` is blocked and
  was not tagged or pushed.
- Session diagnostic directories and files are created/tightened to
  `0700`/`0600`, including pre-existing retained logs. Installer/reinstaller
  now pre-create the launchd log directory/file with the same modes before
  service start.
- Added streaming diagnostic redaction for exact prompts and lines, declared
  secrets, credential-shaped environment values, nested JSON credential
  strings, JSON/base64 encodings, and credential-shaped JSON/env fields.
  Oversize records are replaced rather than persisted uninspected.
- Redaction is diagnostic-only: a focused test proves the canonical replay
  retains the exact original bytes while the mirror contains only a redaction
  marker. The public event/result/receipt/replay schema is unchanged.
- Added `--diagnostic-logs`, `--diagnostic-log-retention`,
  `AGENTD_DIAGNOSTIC_LOGS`, and `AGENTD_DIAGNOSTIC_LOG_RETENTION`. Environment
  overrides win; invalid values fail startup. Defaults are enabled/seven days;
  zero retention means keep indefinitely.
- Startup mode-hardens retained logs and removes only expired regular
  `.ndjson`/`.jsonl` files without following symlinks. An end-to-end native v1
  test proves disabled diagnostics create no log directory/file while the
  immutable terminal receipt remains available.
- Required source gates passed before commit: `go test ./...`, `go test -race
  ./...`, `go vet ./...`, shell syntax, and `git diff --check`. The opt-in
  30-session scenario also passed 30/30 after the change, and the real Docker
  no-plugin process inspection passed.
- Release qualification blocker: building the exact `v2.2.3` Docker images
  exhausted the host data volume. OrbStack stopped and cannot restart because
  the host reports `no space left on device` (231-345 MiB free during later
  checks). Only this repository is writable for this run, so I did not delete
  user caches or OrbStack data outside it. No `v2.2.3` tag, GitHub release, or
  remote source push was created; `origin/main` remains at `v2.2.2`.

### 8. Audit-gap verification

- Complete in source (commit containing this entry). The 2026-08-07 audit was
  checked against the current v2 API rather than assuming its v0.8 paths still
  applied.
- Added a focused two-subscriber broker test: closing one viewer's independent
  subscription does not close the other, and the surviving viewer receives the
  next committed live event. Durable replay remains store-owned rather than a
  viewer-owned shared byte buffer.
- Added a replay/live wire proof with invalid UTF-8 bytes. Authenticated HTTP
  replay, WebSocket catch-up, and WebSocket live delivery all use the same
  unambiguous `raw_base64` field and decode to the exact original bytes. This
  verifies the existing v1 canonical-base64 contract without changing the
  public event schema to reintroduce the retired mixed utf8/base64 framing.
- Added a two-waiter result-broadcast test. Both current consumers observe the
  same closed channel generation; the already-existing multiple-fire test
  proves later results use a fresh generation. The old single-consumer channel
  race was fixed by close-and-replace signaling in `v0.6.2`.
- Fixed the remaining capacity bug using an active-lifetime count under the
  manager lock. Pending/running/orphaned sessions consume admission slots;
  completed/failed records remain inspectable but no longer consume capacity.
  The regression test first failed against the historical `len(map)` behavior,
  then passed after the fix.
- Existing proofs close the other two audit findings: `pkg/runtime/docker_test.go`
  proves native and generic Docker execution publish/query no sidecar port and
  skip the removed sidecar bridge; `pkg/api/routes_retirement_test.go` proves
  the unversioned sidecar route is absent. `pkg/api/auth_test.go` proves missing
  Origin is accepted for non-browser clients, exact same-origin is accepted,
  and cross-origin/invalid-scheme browser requests are rejected.
- Focused new tests, `go test ./...`, `go test -race ./...`, `go vet ./...`,
  and `git diff --check` passed. The first parallel race link hit host ENOSPC;
  serial linking completed green and the exact command then completed green
  from cache. Real Docker repetition was skipped because the recorded
  host-disk/OrbStack blocker remains active; none of this item's proofs require
  a live Docker daemon.

## Phase 3

- Pending; design only after Phases 1 and 2 complete.
