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

- Complete in working tree; commit recorded below after qualification.
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

- Pending.

### 4. Release v2.2.0

- Pending.

## Phase 2

- Pending.

## Phase 3

- Pending; design only after Phases 1 and 2 complete.
