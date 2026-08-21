# AgentD overnight qualification worklog

## v2.3.0 warm resume, retention, and portable provider state

- Started: 2026-08-21 (America/New_York), from released v2.2.5 commit
  `4e316f2619d58732d15c95b272b07df9d90d6bd2`. The installed v2.2.5 daemon
  remains the control while v2.3.0 is developed and qualified. The minor
  version reflects new runtime-lifecycle and portable-resume API contracts.
- Scope authorized: measure the real admission-to-first-output floor; add a
  caller-declared bounded container lease, supervised cleanup of stopped
  AgentD-owned containers, and a static portable provider-state export/import
  path that can cold-boot with a caller-supplied current workspace mapping.
  Preserve immutable per-turn receipts and fail-closed Docker isolation.
- Design boundary: provider conversation state may be retained or exported,
  but credentials, materialized homes, host mounts, and execution-policy
  authority are never included in a portable state bundle. Resume must regrant
  secrets and re-resolve the current `work_dir`/mounts at admission.
- Stretch scope after the core gates: Cursor Agent and Grok CLI native-provider
  support. Provider expansion must not delay or weaken the retention/cleanup
  release.
- Installed v2.2.5 control benchmark (Docker 29.4.0, stamped Codex image,
  auto-discovery disabled to exclude an unrelated malformed host config): HTTP
  admission was 2.127 ms, the Codex thread/turn was ready at 1.668 s, first
  content arrived at 5.521 s, and the minimal turn was terminal at 6.039 s.
  A cold continuation admitted in 3.008 ms, started its resumed turn at 1.845
  s, produced first content at 6.066 s, and completed at 6.468 s. The provider
  accounted for roughly 4.2 s after turn start in both samples; container plus
  provider bootstrap accounted for roughly 1.7-1.8 s.
- The control host had nine stopped AgentD-labeled Codex containers before the
  benchmark; the two new successful turns added two more. Their Docker status
  is `Exited (137)` because v2.2.5 deliberately kills the app-server after
  `turn.completed`; this is not Docker `OOMKilled` proof. No terminal cleanup
  owns ordinary unrestricted containers, so they accumulate indefinitely.
- Warm-path decision: an opt-in maintained container lease makes one logical
  session a multi-turn conversation. `turn.completed` is the durable per-turn
  boundary; the provider transport remains live and accepts the next `prompt`
  through the existing idempotent control ledger. The immutable terminal
  receipt is committed when the lease expires or the caller terminates the
  conversation. This avoids falsifying terminal receipts while allowing
  millisecond prompt dispatch.
- Installed v2.2.5 live-transport prototype proved the target before code
  changes. An interactive Docker Codex session completed its cold first turn,
  then accepted a second prompt through the same app-server transport in
  24.379 ms HTTP wall time. Durable control request/dispatch were committed at
  sequence 24/25; provider `turn/started` followed 27.527 ms after the request,
  first content arrived at 1.741 s wall time, and the complete warm turn ended
  at 1.904 s. The polling observer saw turn-start at 131.889 ms, so the durable
  event timestamps—not poll cadence—prove the 27.5 ms provider dispatch. The
  remaining first-token latency is provider/network inference, not Docker.
- TDD implementation in progress: native Docker requests now accept an
  explicit `container_lease` (`delete` or `maintain`, bounded idle TTL, and an
  optional portable-state snapshot). A maintained transport stays within the
  same durable logical session across prompts, has an independent timeout for
  every provider turn, rejects prompt/steer state mismatches, pins an idle
  volume during snapshot export, and expires to one terminal receipt.
- AgentD now supervises a minute-interval cleanup pass that removes only
  AgentD-labeled Docker containers proven stopped for at least ten minutes;
  running/recent/unproven containers and provider volumes are preserved.
  Ordinary terminal Docker finalization also removes its container and launch
  materialization immediately while preserving retained provider state.
- Portable resume state is a bounded, content-addressed `.agentstate` ZIP
  containing only a provenance manifest and validated provider-state tar.
  Export, latest-state lookup, authenticated download, validated upload, and
  cold import are wired through v1 endpoints. Tar traversal, links/devices,
  cross-provider import, non-Docker use, active-turn snapshots, and restricted
  execution-policy authority widening fail closed. Callers supply fresh
  mounts, current working directory, and secret grants on import.
- Current targeted gates pass for the lease timers/snapshot lock, portable
  store/API round trip, import driver, Docker cleanup, and runtime export/import
  isolation. The installed v2.2.5 control remains untouched.
- Core pre-release gates pass: full `go test ./...`, full `go test -race
  ./...`, `go vet ./...`, dashboard JavaScript syntax, and `git diff --check`.
  The maintained-session integration proves two turns, idempotent warm input,
  idle completion, one terminal receipt, and automatic portable snapshot. The
  cold-import integration proves exact provider identity plus a newly supplied
  working directory.
- Pre-build host check: Docker 29.4.0/overlay2 is healthy; the macOS data volume
  has 200 GiB available. Docker reports 27.7 GB of images, 45.15 GB of volumes,
  and 9.459 GB of build cache, so no pre-build cleanup is required. Installed
  v2.2.5 `/health` remains `ok` with exact `4e316f2...` image stamps and no
  durable sessions in `created`, `starting`, or `running` state.

## v2.2.5 readiness, startup, and Docker continuation

- Started: 2026-08-21 (America/New_York), from the current post-v2.2.4
  dashboard worktree. This is an explicit reviewed v2.2.5 feature/reliability
  release; the installed, verified v2.2.4 daemon on port 38093 remains
  untouched until a v2.2.5 candidate passes its release and live gates.
- Confirmed pre-change implementation causes: `/health` synchronously runs
  Docker daemon discovery, two image inspections, and two OCI-stamp
  inspections under one five-second context; `NetworkManager.EnsureProxy`
  caches its first result (including failure) through `sync.Once`; the shipped
  Squid config contains the invalid `cache_dir null /tmp` directive; and the
  persistence volume overlays only `/home/agent/.claude/projects`, while Codex
  rollout state remains in a fresh per-session `/home/agent/.codex` bind
  mount. Consequently a completed Docker Codex follow-up can receive the old
  provider ID but cannot load its rollout, and cold proxy startup can poison
  the daemon until restart.
- Planned release gates: cached background runtime snapshots with freshness,
  startup proxy prewarm with retryable readiness, provider-correct persistent
  state and public provider identity, immediate durable admission with startup
  progress on the event stream, provider-specific runtime images, then full
  unit/race/release and live installed-daemon qualification.
- TDD implementation completed: Docker proxy readiness now caches only
  success, revalidates cached state, retries transient failures, and allows 45
  seconds for measured cold starts. Docker implements the startup prewarm
  contract; the background monitor probes immediately and every 15 seconds
  with a 60-second bound. `/health` and new admissions read the same snapshot,
  fail stale evidence after 45 seconds, and expose checked time, last error,
  daemon state, and exact image references/digests/OCI stamps. Squid's invalid
  `cache_dir null /tmp` line was removed.
- Docker continuation root causes fixed under tests: Codex persistence now
  overlays `/home/agent/.codex/sessions` (Claude remains
  `/home/agent/.claude/projects`); unrestricted native Docker sessions retain
  provider state by default; durable generation identity and recursive
  `resume_session` lineage choose the root volume after daemon restart; missing
  volumes and Docker provider-ID-only handoffs fail closed. The HTTP path also
  had a second defect: it correctly resolved the provider ID but passed `""`
  into `AttachNativeSessionIO`. A fixture now requires the exact Codex
  `thread/resume` RPC with the original thread ID. Restricted ephemeral policy
  sessions explicitly reject retained-state resume.
- Native POST creation now preserves its HTTP 201 contract but returns as soon
  as the logical session is durably admitted. A supervised, shutdown-cancelled
  activation worker owns materialization, runtime spawn, and provider
  bootstrap. The authenticated session WebSocket emits additive unsequenced
  `session.progress` frames while the existing durable event sequence remains
  unchanged. A blocked-spawn test proves the HTTP response returns in under
  200 ms and live progress reports `runtime.spawn` before release.
- Public session views now expose `provider_session_id`, `resumable`, and
  `resume_source_session_id`. The dashboard enables Continue only when the
  backend reports retained state, always sends the logical AgentD session ID,
  and supports Docker follow-up rather than parsing a provider ID and losing
  its volume lineage.
- Added compatibility, Codex-only, and Claude-only stamped runtime-image
  builds. Qualified AgentD config selects `agentruntime-agent-codex:2.2.5` or
  `agentruntime-agent-claude:2.2.5` per native provider, records the exact
  selected reference/digest in the durable generation, and still builds the
  combined `agentruntime-agent:2.2.5`. Installer and release verification now
  require all three runtime images plus the proxy.
- Pre-image release gates pass: `go test ./...`, `go test -race ./...`,
  `go vet ./...`, JavaScript syntax checks, shell syntax checks, and
  `git diff --check`.
- Release candidate committed and annotated as `v2.2.5` at
  `de173ab0d23ac1214461c986613faa1de26a7258`. A real four-image build
  completed without exit 137 or ENOSPC, with 224 GiB host data-volume
  headroom remaining. Exact image IDs are: compatibility runtime
  `sha256:8ff8d5ccba6be0131fed76b9430bded33e1ee89d1b7a1856b2a4d4025542361f`,
  Codex runtime
  `sha256:400b0ad61ee7e1e3ff2ead9fc169caeeea798dabdc59b7c0c98427929ea9237f`,
  Claude runtime
  `sha256:182d1374e9d283ee09e5b554dc8baf8aaf1a3fd796b4e8cc1f205cbd8632c5ce`,
  and proxy
  `sha256:e48a6ae09de3136cea955c8e6ecb36d88615c74d32e8a410388eb327d3831b86`.
  All four carry exact `2.2.5`/release-commit OCI stamps; runtime provider
  labels are `all`, `codex`, and `claude` respectively.
- Image-content qualification confirms the compatibility image contains Codex
  0.149.0, Claude Code 2.1.238, and `/usr/bin/bwrap`; the 727.3 MB Codex image
  contains Codex and bubblewrap but no Claude CLI; the 779.6 MB Claude image
  contains Claude but neither Codex nor bubblewrap. The compatibility runtime
  is 1.063 GB and the proxy is 236.4 MB. The release verifier passed with both
  `REQUIRE_RELEASE_TAG=1` and `VERIFY_DOCKER_IMAGES=1` before installation.
- Follow-up packaging decision: provider CLIs release independently and often
  daily, so future images should use content-addressed provider base images
  promoted on their own tested cadence. AgentD releases should add only a thin
  ABI/provenance layer pinned to a provider digest; heavy toolchain layers must
  rebuild only when provider or sandbox inputs change, with the previous
  digest retained for rollback. This is not being added after the v2.2.5 tag.
- The first restamp attempt proved that `AGENTD_VERSION`/`AGENTD_COMMIT` were
  declared before the runtime package `RUN`, invalidating the entire heavy
  provider layer for a metadata-only commit change. That redundant build was
  cancelled before completion. A red Dockerfile-order regression test now
  requires release stamp arguments after the final content layer; v2.2.5 keeps
  the already-qualified provider/sandbox layers byte-for-byte and changes only
  OCI provenance. Fully independent daily provider-base promotion remains a
  later release concern.
- Final candidate `v2.2.5` is commit
  `40a00ef91869fd4922fe8cc81b89e1c4c28f076f`. Its real cache-transition build
  completed without exit 137 or ENOSPC; a second identical four-image build
  showed every provider/package/bubblewrap layer as `CACHED` and completed in
  21.63 seconds. Final image IDs are compatibility
  `sha256:acb203c893b8aebc2fa400a9f88682d8b6ca0604fed03733c4dc1fa8d52b7743`,
  Codex `sha256:0149bac5be1abcd6abac5c1f67b4b3bc4984fbef3087d0b4c7e0e5bd63d572f7`,
  Claude `sha256:c0cd7f581693c0f639ab777934d46a49ff462cfa844b1c8107ce43e90e24d7f2`,
  and proxy
  `sha256:43889d69cae09c193c2b2aad744f427415df6afd486a341a0876e9975545d90d`.
  All four carry the exact final version/commit OCI stamps, the tag-required
  release verifier passed with Docker image verification enabled, and host
  data-volume headroom remained 220 GiB.
- The first run of the replacement-style installer correctly stopped the old
  job but immediate bootstrap failed verbatim with `Bootstrap failed: 5:
  Input/output error`; launchd no longer listed the service and port 38093 had
  no listener. The plist passed `plutil -lint`, and the same bootstrap command
  succeeded after launchd's asynchronous teardown completed, proving a bounded
  bootout/bootstrap race. A red installer regression now requires a named
  bounded retry; bootstrap retries once per second for at most 30 attempts,
  preserves fail-loud behavior after the bound, and only then kickstarts.
- Live health after manual service restoration found
  `"docker":"unavailable","local":"stale"` with Docker's exact error
  `docker proxy did not become ready: context deadline exceeded`. The earlier
  passive-local fix was correct in isolation, but the monitor still ran all
  runtimes as one batch and waited for the 60-second Docker probe before its
  next interval; local therefore aged past 45 seconds behind Docker. A red
  concurrency regression reproduced this. Each runtime now owns an independent
  supervised refresh loop, so Docker latency cannot block local freshness or
  monitor shutdown.
- The first installed v2.2.5 candidate at `de173ab...` was rejected by the live
  gate and preserved as an RC rather than attested. The installer replaced the
  executable and plist but `launchctl load` returned verbatim `Load failed: 5:
  Input/output error`; PID 34416 remained the in-memory v2.2.4 process and
  `/health` still reported `"version":"2.2.4"` until an explicit kickstart.
  Installer regression coverage now requires bootout/bootstrap/kickstart so an
  upgrade replaces an already-loaded job instead of printing false success.
- After the restart, the passive monitor exposed a second exact failure:
  `/health` returned HTTP 503 with `"docker":"ready","local":"stale"`.
  Root cause: `runtimeReadinessMonitor.refresh` skipped runtimes that do not
  implement `AdmissionChecker`, so local's constructor timestamp was never
  refreshed and crossed the 45-second stale threshold. A red test reproduced
  the unchanged timestamp; the monitor now republishes a fresh ready snapshot
  for passive runtimes on every interval.
- The live upgrade also found `agentruntime-proxy` still running the v2.2.4
  image ID `sha256:a21efddc...` under the mutable `:latest` reference. The new
  manager revalidated liveness/readiness but not provenance, so it accepted the
  old process while `/health` correctly described only the installed v2.2.5
  image. A red test now requires replacement when the running container's
  immutable image ID differs from the configured proxy image ID. The sole
  AgentD-owned stale proxy container was removed and recreated from stamped
  v2.2.5; no unrelated container, host file, or cache was deleted.
- Installed-candidate live Docker session
  `2db6b3a1-d39e-4f43-a8c7-73225ccf02af` proved a first-generation persistence
  regression. Admission returned HTTP 201 in 6.193 ms and the WebSocket emitted
  `runtime.spawn`, then the session failed verbatim with `spawn: docker run
  args: persistent provider volume
  "agentruntime-vol-2db6b3a1-d39e-4f43-a8c7-73225ccf02af" is unavailable: exit
  status 1: []\nError response from daemon: get
  agentruntime-vol-2db6b3a1-d39e-4f43-a8c7-73225ccf02af: no such volume`.
  Root cause: the API stored the deterministic new volume name and also passed
  it through `SpawnConfig.VolumeName`, whose runtime contract means an existing
  resume volume; Docker therefore inspected a volume generation 1 had not yet
  created. A red volume-plan regression now distinguishes the durable name
  from the existing-volume runtime hint. Generation 1 creates the volume;
  continuations reuse and validate it, preserving fail-closed missing-state
  behavior.
- The rejected installed candidate was preserved as annotated tag
  `v2.2.5-rc4`. Final local release tag `v2.2.5` now points to
  `4e316f2619d58732d15c95b272b07df9d90d6bd2`. Its first-generation volume
  regression failed Red before the canonical volume-plan helper passed, then
  `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`,
  shell/JavaScript syntax, and `git diff --check` all passed.
- Final four-image restamp completed in 36.21 seconds. Every heavy provider,
  package, user, and bubblewrap content layer was `CACHED`; only the
  version/revision image configuration changed. Exact final image IDs are
  compatibility runtime
  `sha256:32ef3b7ee442bb9e94722c06c789348e7f30a7032e8ca47f691b481e85953b14`,
  Codex runtime
  `sha256:6b1a2844be8f7012c02c51ebb3867ede9e08b404dc09e9436df2d3be058e820e`,
  Claude runtime
  `sha256:9c05711231d9fdaeee4459998b5f08f0af64eb93ded6364b97ec129d2c1c63d7`,
  and proxy
  `sha256:b999f29f5fd7c4deb15214a6bb3b0592f47a38e0ed48c3d088ed23e332ba4f5b`.
  All exact OCI stamps and provider labels passed
  `REQUIRE_RELEASE_TAG=1 VERIFY_DOCKER_IMAGES=1 scripts/verify-release.sh
  2.2.5 4e316f2619d58732d15c95b272b07df9d90d6bd2`. Docker/OrbStack reported
  client/server 29.4.0 and host data-volume headroom was 220 GiB before the
  gates and 216 GiB afterward; no user or repo cache was deleted.
- The corrected installer completed with `--port 38093 --docker-default` and
  launchd now runs exact installed binary
  `2.2.5@4e316f2619d58732d15c95b272b07df9d90d6bd2`. Initial health became HTTP
  200 on attempt 9. More than 45 seconds later, three consecutive `/health`
  reads took 0.78-1.23 ms and returned fresh independent snapshots:
  `status=ok`, `default_runtime=docker`, Docker/local `ready`, all four exact
  image digests/stamps above, and `stale=false`. The live shared proxy is
  running immutable image ID `sha256:b999f29...`.
- Live installed-daemon streaming/steering proof: Docker Codex session
  `63d92724-8681-4a4f-bcf1-c1b75622b37f` admitted HTTP 201 in 2.573 ms,
  streamed spawn/bootstrap/running progress, 65 durable events, 30 content
  deltas, and full normalized tool payloads. An automated steer sent at the
  first turn-start notification was transiently too early and returned HTTP
  503; the same manual mid-turn action was accepted HTTP 202 and committed.
  The session completed exit 0 with
  `STREAM_STEER_MARKER ORIGINAL_CONTEXT_NONCE_P4M8 MANUAL_STEER_WORKED`, output
  hash `sha256:d03da95cbcbb4e6113257e1a99776d2e1200ed514dfbd21068607f7bba45373e`,
  provider ID `01a022fb-cdf2-7ca3-9f2d-4da66fca91a3`, and `resumable=true`.
- Installed-daemon history continuation proof: follow-up
  `b04ab6f1-b034-476c-ac98-47b1dfee6b4d` resumed the logical AgentD ID above,
  reused the same provider ID and root volume
  `agentruntime-vol-63d92724-8681-4a4f-bcf1-c1b75622b37f`, streamed 36 events
  and 13 deltas, recalled `ORIGINAL_CONTEXT_NONCE_P4M8`, and completed exit 0
  with `resume_source_session_id` set to the root session.
- Live installed-daemon restricted-egress positive: session
  `0237769e-7965-4a7c-826a-8d6360c1551a` admitted in 3.266 ms, streamed the
  exact structured result `{"ok":true}`, and completed exit 0 at sequence 26
  with output hash
  `sha256:db9b5fef29183b1ca8d8d6d41e288da06e4b14b8e70847a3159b3a24f401312c`
  and artifact hash
  `sha256:4062edaf750fb8074e7e83e0c9028c94e32468a8b6f1614774328ef045150f93`.
- Live installed-daemon missing-auth-host negative: the final counted session
  `00a83203-06d9-40c6-8808-eabf120900ef` used only an in-memory near-expiry
  credential copy and omitted only `auth.openai.com`. It failed exit 1 at
  sequence 136 with exact terminal attribution `restricted_egress:
  egress_denied: egress_denied: CONNECT host "auth.openai.com"`, with no
  context-deadline substitution. A first 90-second attempt reached the same
  denied-host attribution just as its caller timeout settled; a later retry
  failed preflight when OrbStack's Docker socket disappeared. OrbStack
  automatically restarted its VM and unrelated containers; AgentD changed its
  cached Docker snapshot to unavailable/HTTP 503, kept local fresh, and
  self-recovered to HTTP 200 without an AgentD restart once Docker returned.
- Port 38094 dashboard handoff is running as launchd job
  `com.agentruntime.dashboard-dev`, PID 24751 at verification, from the exact
  final installed binary. `/dashboard/` and `/health` are HTTP 200. A T3 browser
  DOM check loaded both dashboard scripts and proved the Console exposes
  Codex/Claude, model choices, all effort tiers, Docker/local, timeouts, trace,
  advanced controls, streaming activity, steering, cancellation, and History.
  Port 38094 authentication uses
  `.artifacts/dashboard-dev/data/auth.token` (the installed 38093 daemon uses
  `~/.agentd/auth.token`); the browser retains it only in that tab's session
  storage.
- Daily provider CLI policy: do not clone mutable running containers. Keep
  independently tested, content-addressed Codex and Claude base layers and
  promote only the provider whose CLI/sandbox inputs changed; AgentD release
  images should remain thin digest-pinned provenance wrappers. v2.2.5 already
  makes daemon-only restamps reuse all heavy content. Fully independent daily
  provider-base publishing remains a separately reviewed release pipeline.
- Remaining Trading Floor promotion blocker: the installed floor health
  endpoint is HTTP 200 but reports `agent_runtime=unavailable` because its
  installed adapter still intentionally pins exact
  `2.2.4@a5d0560c68c2b9f60629b623e137146f9d1149ca`. The Trading Floor worktree has
  an existing overlapping `IN_PROGRESS` Slice 042 with uncommitted changes and
  a contract that forbids silent scope expansion. No floor source, installed
  package, or service was modified. A reviewed v2.2.5 consumer-pin slice and
  service promotion are required before the floor can truthfully report
  `agent_runtime=healthy`.
- **AgentD readiness attestation (floor promotion pending):** installed AgentD
  `2.2.5@4e316f2619d58732d15c95b272b07df9d90d6bd2`; runtime digests
  `all=sha256:32ef3b7e...`, `codex=sha256:6b1a2844...`,
  `claude=sha256:9c057112...`, proxy `sha256:b999f29f...`; live `/health` is
  HTTP 200 in under 2 ms with `status=ok`, Docker/local `ready`, exact fresh
  image stamps, and `stale=false`.

## Embedded live-agent console scaffold

- Console activity now renders expandable normalized tool/provider items with
  method, item kind, status, optional duration, and the full JSON payload.
  Terminal local sessions can be continued from either the composer or a new
  History `Continue` action. The initial scaffold deliberately disabled Docker
  continuation after two false-continuation proofs exposed fresh per-session
  mounts; the v2.2.5 persistence and lineage implementation above supersedes
  that safeguard and re-enables the control only when the backend reports the
  session as resumable.
- Docker cold-path measurements: the runtime image is 1.048 GB and the proxy
  image is 236 MB. The observed first failure was dominated by Squid readiness
  (33 seconds), while subsequent proxy-warm Docker creates still took roughly
  14-22 seconds before admission returned. The highest-value v2.2.5 changes are
  startup-time proxy prewarming, retryable proxy readiness, removing the invalid
  Squid `cache_dir null` initialization path, background/cached admission
  snapshots, and then provider-specific slimmer runtime images. A warm container
  pool is not recommended until credential/mount isolation can be proven.
- First dashboard Docker launch `9f2d8ba9-72cb-4396-886b-6d366f0d53af`
  failed pre-spawn at sequence 1 with verbatim error `spawn: docker proxy:
  docker proxy did not become ready: context deadline exceeded`. The proxy
  container started at 03:50:25Z but Squid did not accept port 3128 until
  03:50:58Z, after AgentD's readiness wait expired. `NetworkManager.EnsureProxy`
  caches that first error through `sync.Once`, so the otherwise-ready proxy was
  not retryable within the same daemon process. No code was hotfixed; the
  isolated dashboard-dev job alone was restarted to clear the cached failure.
  A subsequent Docker proof session
  `183ae356-c930-4c88-ac95-94c41466dcbd` completed with exit 0 at sequence 25
  and streamed exact output `DOCKER_DASHBOARD_OK`. Making proxy readiness
  retryable is a reviewed v2.2.5 code change.
- The initial retained-terminal preview on port 38094 ended with its tool
  session, so it was not a durable handoff. Replaced it with the separate
  launchd job `com.agentruntime.dashboard-dev`, serving the current repo build
  from `.artifacts/dashboard-dev` on loopback port 38094 with local as its
  default runtime. Launchd reports it running as PID 59876; `/health` is HTTP
  200 and the served dashboard contains the console/auth assets. It is isolated
  from installed v2.2.4 PID 34416 on port 38093.
- Readiness follow-up decision: v2.2.5 should supervise Docker admission probes
  in the background and publish a timestamped last-known snapshot. `/health`
  should only read that snapshot, report freshness/staleness, and return
  immediately; it should not synchronously run the five Docker CLI checks per
  request. This is an explicit reviewed-release behavior change, not a v2.2.4
  deployment hotfix.
- Follow-up health incident at 2026-08-20 23:23 America/New_York: the installed
  v2.2.4 daemon on port 38093 remained the original launchd PID 34416 and
  launchd reported that it had never exited, but one `/health` request produced
  no bytes before the client's 5.006-second deadline. Docker itself remained
  reachable. Without restarting or changing the installed daemon, the next
  three `/health` probes returned HTTP 200 with Docker/local ready in
  0.60-1.10 seconds, and Trading Floor `/api/v1/health` returned HTTP 200 with
  `agent_runtime="healthy"`. This was another transient Docker readiness-latency
  event, not an AgentD crash. The isolated dashboard dev daemon used port 38094,
  was no longer running at incident time, and its uninstalled repository assets
  cannot affect the immutable v2.2.4 binary on port 38093.
- Added a repo-native Console tab to the authenticated embedded dashboard rather
  than introducing a second web stack. It launches native Codex or Claude
  sessions through `POST /api/v1/sessions`, streams durable events over the
  authenticated session WebSocket, renders `content.delta` output and tool
  activity live, and exposes steer, interrupt, and cancel controls.
- Launch configuration includes model-aware effort choices, supported fast mode,
  local/Docker runtime, timeout, trace policy, clean/continued context, workdir,
  Claude max turns, optional structured-output schema, and opt-in Codex
  restricted egress. Restricted auth JSON is parsed from a file and retained in
  memory only. The dashboard bearer token now uses a non-blocking in-page gate
  and session storage; it is never put in local storage or a URL.
- Live isolated-daemon proof on 2026-08-20: the Console form launched local
  Codex session `1f6bcb65-dc94-426f-9b3e-fc58c555285b` with
  `gpt-5.6-luna`/`medium`; the durable stream reached 872 contiguous events.
  Mid-turn steer `dashboard-live-steer-1` was durably recorded as
  `control.steer.requested` at sequence 459 and
  `control.steer.dispatched` at sequence 460. The session completed at sequence
  872 with exit 0 and streamed the requested terminal marker
  `STREAM_TEST_COMPLETE`. This used a separate dev daemon on port 38094 and did
  not alter or restart the installed v2.2.4 daemon on port 38093.

## v2.2.4 installed-deployment readiness repair

- Started: 2026-08-20 15:23 America/New_York from clean `main` at
  `5c3fdd1fd2a88a1df96168aabdc3bbf564e23cda`; deployment repair only. The
  immutable release source is tag `v2.2.4` at
  `a5d0560c68c2b9f60629b623e137146f9d1149ca`.
- Pre-fix binary identity was already exact: `~/.local/bin/agentd --build-info`
  returned `{"agentd_version":"2.2.4","commit_hash":"a5d0560c68c2b9f60629b623e137146f9d1149ca"}`
  and `--require-build` passed. The live launchd service is
  `com.danieliser.agentd`, PID 47885, loopback `127.0.0.1:38093`, Docker
  default, using `~/.agentd`.
- Pre-fix exact images were present and correctly stamped:
  `agentruntime-agent:2.2.4` was
  `sha256:83806fd3ffb840873757c209e38c6278f454765258d324788110019cbda9c4e1`
  and `agentruntime-proxy:2.2.4` was
  `sha256:15c9be9c041be15a8accf1481868b6686b8e1fc033bc1ce39dd489dc077ef16e`;
  both labels were exactly `2.2.4` and the pinned commit. Direct inspection of
  the runtime image found `/usr/bin/bwrap`, `bubblewrap 0.8.0`.
- OrbStack/Docker was running (client/server 29.4.0, `orbstack` context) and
  ordinary `docker ps`/exact image inspections completed in 0.08-0.16 seconds
  when uncontended. Host data-volume headroom was 272 GiB (85% used), so no
  disk or cache deletion was needed before repair.
- **Actual pre-fix cause, recorded before repair:** the canary's retained
  verbatim failure is `AGENTD_RUNTIME_UNAVAILABLE: AgentD Docker runtime is
  not explicitly ready for admission.` It ran from 18:58:10.324657Z to
  18:58:15.493750Z, created no AgentD session, and coincided with the live
  `/health` Docker admission probe exhausting its single five-second bounded
  context. AgentD's probe performs `docker ps`, two exact image inspections,
  and two OCI-label inspections sequentially inside that one bound; `/health`
  suppresses the inner command error, so the most exact available mechanism is
  a transient Docker CLI/daemon response overrun, not a missing/misstamped
  binary or image and not ENOSPC. This was independently observed as HTTP 503
  with verbatim body
  `{"default_runtime":"docker","runtime_status":{"docker":"unavailable","local":"ready"},"runtimes":["docker","local"],"status":"error","version":"2.2.4"}`;
  it returned HTTP 200/`docker:ready` without any artifact change once Docker
  calls were responsive. No more specific inner Docker error was persisted by
  the current health endpoint, and this worklog does not invent one.
- Rebuilt from the detached immutable release worktree at the pinned commit.
  `agentruntime-agent:2.2.4` used `docker build --no-cache --pull`; its package
  layer ran for 1,805.1 seconds and its separate bubblewrap layer ran for 50.5
  seconds, proving this was not reuse of the prior 2.2.x runtime contents. The
  resulting image ID is
  `sha256:14ddadadecedeac1220059d3e82aae20f521d372129a9d83e38b01b0ca09a73f`.
  `agentruntime-proxy:2.2.4` was also rebuilt with `--no-cache --pull`; its new
  image ID is
  `sha256:a21efddc95aca6371be5325901d86c45723675ce7cf5146e016cff5e9cafaf30`.
  Both carry exact `2.2.4`/pinned-commit OCI labels. The rebuilt runtime
  directly reports `/usr/bin/bwrap`, `bubblewrap 0.8.0`, Debian package
  `0.8.0-2+deb12u1`. Final build-time disk headroom remained 267 GiB; no cache
  or data was deleted. Heavy unrelated Airbyte/ClickHouse container load made
  the uncached toolchain layer slow, but it completed without exit 137 or
  ENOSPC and those workloads were not altered.
- Installation: unloaded the previous ad-hoc `com.danieliser.agentd` launchd
  job, then ran the pinned release's installer with `--port 38093
  --docker-default --no-credential-sync`. The canonical
  `com.agentruntime.agentd` job is live as PID 34416, loopback-only, using the
  existing `~/.agentd` data directory. Its newly built installed binary again
  passes `--require-build 2.2.4@a5d0560c68c2b9f60629b623e137146f9d1149ca`.
  `REQUIRE_RELEASE_TAG=1 VERIFY_DOCKER_IMAGES=1
  scripts/verify-release.sh 2.2.4
  a5d0560c68c2b9f60629b623e137146f9d1149ca` passed.
- Live readiness proof: the installed daemon returned HTTP 200 ten consecutive
  times immediately after install with exact body
  `{"default_runtime":"docker","runtime_status":{"docker":"ready","local":"ready"},"runtimes":["docker","local"],"status":"ok","version":"2.2.4"}`.
  Its authenticated capabilities returned `agentd_version=2.2.4`, the pinned
  `commit_hash`, Docker in `runtimes`, execution-policy versions 2.0/2.1, and
  the exact Codex endpoints. After qualification and its Docker cleanup, the
  same `/health` payload was still HTTP 200. A fresh post-TTL request to the
  live Trading Floor daemon at `/api/v1/health` reported
  `services.agent_runtime="healthy"` twice; unrelated market-data,
  intelligence, and optional OpenTraces services remain degraded/unavailable.
- Live installed-daemon positive gate: session
  `6d06f213-b2fb-4ffd-970b-c2f6d3800563` completed the opt-in restricted-egress
  Codex turn with exact result `{"ok":true}`, exit 0, last sequence 26,
  artifact hash
  `sha256:4062edaf750fb8074e7e83e0c9028c94e32468a8b6f1614774328ef045150f93`,
  and output hash
  `sha256:452b61a8167c605f9975ca6fe9104bde449c2dce8a6d5d4ccd89d7ad61cf5fde`.
  Before the counted successful gate, one local YAML request was rejected
  before dispatch because the CLI cannot decode its raw JSON schema field,
  and two JSON attempts were timing-invalidated by a 30-second client cancel
  and then host-starved Squid startup. Their exact terminal error was
  `restricted_egress: egress_preflight_failed: egress_preflight_failed: policy
  proxy unavailable`; only their explicitly session-labeled orphan proxy and
  network resources were removed. No source or unrelated workload changed.
- Live installed-daemon missing-auth-host negative: session
  `21584c33-a970-4f4b-bfb8-3cc604b9a4f0` used an isolated near-expiry auth copy
  and removed only `auth.openai.com`. It failed as required with exit 1 and
  exact terminal attribution `restricted_egress: egress_denied: egress_denied:
  CONNECT host "auth.openai.com"`; there was no `context deadline exceeded`.
  The private egress evidence contained only `timestamp` and `connect_host`
  fields and included the denied host. Its failed receipt is durable (last
  sequence 137, output hash
  `sha256:1dd212d19c9021bd3192475dd430fc8d7243a2c4ae146cbdbd02f21ae6d0fc6a`).
- **Readiness attestation (Trading Floor cite):** installed AgentD
  `2.2.4@a5d0560c68c2b9f60629b623e137146f9d1149ca`; runtime/proxy content digests
  `sha256:14ddadadecedeac1220059d3e82aae20f521d372129a9d83e38b01b0ca09a73f` /
  `sha256:a21efddc95aca6371be5325901d86c45723675ce7cf5146e016cff5e9cafaf30`;
  live `/health` is HTTP 200 `status=ok`, `default_runtime=docker`,
  `docker=ready`, `local=ready`, and a fresh Trading Floor capability
  handshake reports `agent_runtime=healthy`.

## v2.2.4 restricted-egress provider-turn qualification

- Started: 2026-08-20 (America/New_York), from `main` at `07a5093` with a
  clean worktree.
- Required evidence read: this worklog, CHANGELOG v2.2.0-v2.2.3, and Trading
  Floor Slice 038 including Dispositions. Retrieved retained session
  `8bd10dc2-263d-4456-96ea-1cac64129de4` from `~/.agentd/agentd.sqlite`: exact
  two-host policy/hash, sole sequence-1 `session.failed` event with
  `context deadline exceeded`, matching failed receipt, exit -1, and the empty
  SHA-256 output hash. The retained private Squid policy config contains only
  `api.openai.com` and `chatgpt.com`; no provider-byte diagnostic existed.
- Untouched baseline: `go test ./...` PASS and `go test -race ./...` PASS.
- Diagnostics Red: focused tests failed because the execution policy had no
  opt-in diagnostic field, capabilities did not advertise it, the Squid policy
  renderer accepted no diagnostic mode, and no private CONNECT record writer
  existed.
- Diagnostics Green: added hash-covered, default-off
  `egress_diagnostics`; capability metadata names the flag/default and the only
  retained fields (`timestamp`, `connect_host`). The proxy format excludes
  methods, URLs, payloads, and headers; parsed records use the existing private
  diagnostic directory and retention lifecycle. Focused tests, `go test ./...`,
  `go test -race ./...`, and `git diff --check` pass.
- Reproduction Red: the opt-in real-Docker gate ran the actual Codex CLI 0.148.0
  app-server with the canary's exact `api.openai.com` + `chatgpt.com` allowlist,
  2 GiB/2 CPU/256 PID/1,024-FD ceilings, explicit one-session auth, and a
  minimal structured-output turn. It failed after 19.19 seconds with an empty
  CONNECT diagnostic file: the provider attempted no proxy connection before
  AgentD's 15-second native bootstrap context expired.
- **Root cause (verbatim):** `Codex could not find bubblewrap on PATH. Install
  bubblewrap with your OS package manager. See the sandbox prerequisites:
  https://developers.openai.com/codex/concepts/sandboxing#prerequisites. Codex
  will use the bundled bubblewrap in the meantime.` In an unrestricted
  container, the same actual app-server took approximately 40 seconds before
  returning its `initialize` response and that warning. AgentD killed it at 15
  seconds and persisted only `context deadline exceeded`. Missing allowlist
  hosts, proxy-environment non-compliance, and a direct-DNS attempt were not the
  first failure; there were zero CONNECT attempts.
- Provider-path finding (verbatim): `Codex's model request honored the injected
  HTTP(S) proxy, but its ChatGPT token-refresh client made no proxy CONNECT
  until AgentD launched app-server with the provider-native setting
  features.respect_system_proxy=true. With that setting enabled, the same
  near-expiry credential attempted CONNECT auth.openai.com and the exact-host
  proxy denied it.` The Codex provider endpoint menu therefore requires both
  `auth.openai.com` (OAuth refresh) and `chatgpt.com` (turn transport);
  `api.openai.com` remains the separately advertised `web_search` endpoint.
- Fix Green: the agent image installs system `bubblewrap`; native
  startup remains bounded at 60 seconds; restricted Codex launch forces its
  native proxy-resolution feature; every allowlisted host receives a bounded,
  retry-once CONNECT preflight before provider launch; and durable launch
  activation receives a fresh timeout after preflight rather than inheriting
  an already-consumed 10-second database context. Failures classify as
  `egress_preflight_failed`, `egress_denied`, or `provider_startup_failed`.
- Qualification Red/Green: the new opt-in paid Docker gate first reproduced
  the empty-output deadline. Its positive case now completes an actual Codex
  app-server turn with strict JSON output and a durable artifact/receipt under
  the exact three-host policy. Its negative case uses an isolated near-expiry
  copy of the credential, removes only `auth.openai.com`, and now commits a
  named `egress_denied` failure plus timestamp/host-only CONNECT evidence; the
  denied request never reaches the auth service and cannot rotate the retained
  refresh token.
- Final exact-stamp gate: `v2.2.4` source and both OCI images identify commit
  `a5d0560c68c2b9f60629b623e137146f9d1149ca`; image-aware
  `verify-release.sh` passed. The combined real-Docker qualification passed:
  complete turn in 71.96 seconds and named missing-auth failure in 63.24
  seconds. `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `git diff --check` were green for every source commit.
- Public result, receipt, event, replay, and authentication contracts are
  unchanged. Additive surfaces are the hash-covered diagnostic policy field,
  capability metadata, endpoint menu entry, and stable failure codes.

## Final summary

- **Canary pin:** Trading Floor should review and pin exact release `v2.2.4`
  at commit `a5d0560c68c2b9f60629b623e137146f9d1149ca`.
- **Root cause (verbatim):** `Codex could not find bubblewrap on PATH. Install
  bubblewrap with your OS package manager. See the sandbox prerequisites:
  https://developers.openai.com/codex/concepts/sandboxing#prerequisites. Codex
  will use the bundled bubblewrap in the meantime.` That slow startup crossed
  AgentD's 15-second bootstrap bound before any CONNECT. Once startup was
  repaired, Codex OAuth refresh additionally required the exact endpoint
  `auth.openai.com` and the provider-native setting
  `features.respect_system_proxy=true`; environment proxy variables alone did
  not route that client.
- **Minimal Trading Floor policy delta:** retain the existing
  `api.openai.com` and `chatgpt.com` hosts and exact v2.2.3 ceilings, add only
  `auth.openai.com`, set `egress_diagnostics: true` for the qualification
  canary, and recompute the hash over that final policy. No authority or
  contract check is relaxed.
- **Phase 1 complete:** policy egress `f14f88c`, truthful image readiness
  `95f2b5a`, no-plugin process proof `1610aa7`, and release `v2.2.0` at
  `530bad2b5dab578589bf422c4573d6f3182f2389`.
- **Phase 2 complete:** resource ceilings/release `v2.2.1` at `b4deea2`,
  30-session proof/release `v2.2.2` at `5dca0fc`, diagnostic hygiene
  `d537d2b`, retained-audit closure `393858a`, and explicit Docker-only
  recovery/release `v2.2.3` at `117064b887d3292f582c56d14761addf2e12c9f8`.
- **Phase 3 complete as design only:** `4490198` documents the provider
  headless/SDK migration and authenticated private-work surface, including the
  threat model and owner decisions required before code.
- **Qualification:** every commit was gated by green `go test ./...` and
  `go test -race ./...`; final vet/diff checks passed. v2.2.3 GitHub CI passed
  on Go 1.24.2/1.26.x and all wheels published successfully to PyPI. Exact
  stamped-image verification and real Docker egress, readiness, no-plugin,
  resource-ceiling, and OOM tests passed.
- **Final concurrency measurement:** completed 30/30; latency p50 1.294 s,
  p95 1.308 s, max 1.315 s; peak AgentD RSS 48,736 KiB, 136 open FDs, and 61
  processes. Private artifacts:
  `.artifacts/concurrency/20260820T092931Z-29901/`.
- **Skipped by design:** local-process restart recovery was not invented; the
  versioned capability explicitly reports it unsupported. Phase 3 contains no
  implementation. A live Trading Floor Source Scout was not dispatched and
  its exact pin was not edited because Trading Floor was read-only; its fresh
  candidate-only canary is the next consumer action.
- **Build exception:** a fresh unpinned runtime-image rebuild was killed with
  exit 137 during package installation. Since items 7-9 do not change runtime
  image contents, v2.2.3 uses the already-qualified v2.2.2 root filesystem
  layers byte-for-byte with new exact v2.2.3 OCI version/revision metadata.
  This avoided an unreviewed vendor-CLI drift; layer equality and stamps were
  verified before release.

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

- Complete in source commit `d537d2b`; released with items 8-9 as `v2.2.3` at
  `117064b887d3292f582c56d14761addf2e12c9f8`.
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
- Release qualification was temporarily blocked when the host data volume
  filled and OrbStack could not restart. I did not delete user caches or
  OrbStack data outside this repository. Standard reachable-object `git gc`
  inside this repository preserved all refs/worktree files and released enough
  APFS space for exact source, Docker, and release gates to finish.

### 8. Audit-gap verification

- Complete in commit `393858a`. The 2026-08-07 audit was
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

### 9. Non-Docker restart recovery

- Complete in commit/release `117064b887d3292f582c56d14761addf2e12c9f8`
  (`v2.2.3`) by choosing the explicit unsupported path; no local-process
  recovery semantics were invented.
- Added additive, versioned recovery capability `1.0`. When and only when the
  durable store, event broker, and Docker runtime are present, it reports
  `daemon_restart: docker_only`, `supported_runtimes: [docker]`, and lists
  `local` under `unsupported_runtimes`. Without the complete reconstruction
  proof it reports `daemon_restart: unsupported` and an empty supported list.
- The Go client decodes the new capability so callers can gate work directly.
  Existing `replay.restart_persistence` and `docker_reconstruction` fields are
  unchanged; replay durability is no longer sufficient for a caller to infer
  local-process restart recovery.
- Focused server/client tests cover both docker-only and fail-closed unsupported
  advertisements. Local runtime's existing test continues to prove `Recover`
  returns no handles after a daemon restart.
- The exact stamped v2.2.3 agent/proxy images have byte-identical root
  filesystem layer lists to the qualified v2.2.2 images because items 7-9 are
  daemon-only. A fresh unpinned provider-image rebuild was killed with exit 137
  during package installation, so reusing the already-qualified filesystem
  avoided silently pulling different vendor CLIs. Only the OCI version/revision
  config changed, and `verify-release.sh` proved both exact stamps.
- Real Docker egress/default-deny/direct-bypass, resource/OOM, image-readiness,
  and no-plugin process tests passed against the stamped images. The final
  30-session run completed 30/30 with p50 1.294 s, p95 1.308 s, max 1.315 s,
  peak RSS 48,736 KiB, 136 FDs, and 61 processes; artifacts are under
  `.artifacts/concurrency/20260820T092931Z-29901/`.
- GitHub CI passed on Go 1.24.2 and 1.26.x. All platform wheels built and the
  trusted PyPI publish completed successfully.

## Phase 3

- Complete as design only in root `DESIGN_NOTES.md`; no adapter or private-work
  code was implemented.
- Provider migration notes keep `nativeprotocol.Transport` as the sole seam,
  require deterministic canonical-record compatibility, put validate/retry
  behind a new hash-covered policy version, and preserve explicit resume and
  indeterminate crash semantics.
- Private admission notes define a consumer-neutral signed caller proof,
  versioned private endpoint/capability, atomic nonce/idempotency checks,
  separate AgentD-signed acceptance, private payload rules, threat model,
  review paths, and negative qualification gates. Trading Floor is only the
  initial caller assumption; no trading-domain authority enters AgentD.
- Owner review is explicitly required before implementation. Open decisions
  are caller OS identity, encrypted-versus-unpersisted private bodies, and
  first-version key rotation/multi-caller scope.
