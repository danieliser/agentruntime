# Test Suite Audit Report

**Date:** 2026-03-19
**Philosophy:** "Write tests. Not too many. Mostly integration." (Kent C. Dodds)

## Summary

- **Total Test Functions:** 576
- **Test Files:** 65
- **Average Tests per File:** 8.9

The test suite is **well-structured with strong integration coverage**, but contains notable bloat in specific areas. This audit identifies redundant unit tests, missing critical paths, and opportunities to rebalance toward higher-value integration tests.

---

## Package-by-Package Analysis

### cmd/agentd (3 tests) — HEALTHY
**Files:** `dispatch_test.go`, `main_test.go`

- **Tests:** 3
- **Assessment:** Minimal; focuses on integration of daemon dispatch logic
- **KEEP:** All tests
  - `TestDispatch_CreatesAndTracksSession` — validates session creation integration
  - `TestMain_BindsPort` — HTTP listening verification

**Status:** ✅ No action needed. Lean, focused.

---

### cmd/sidecar (48 tests) — MIXED SIGNALS, SOME TRIM OPPORTUNITIES

#### agentconfig_test.go (18 tests) — TRIM CANDIDATES

**Problem:** Excessive unit test coverage of trivial JSON deserialization. Tests validate behavior the Go type system already guarantees.

**KEEP (6 tests — critical validation):**
- `TestParseAgentConfig_InvalidJSON_ReturnsError` — required for error handling boundary
- `TestParseAgentConfig_ShellInjectionInModel_NotExecuted` — **SECURITY**
- `TestParseAgentConfig_EnvWithSpecialChars_PassThrough` — **SECURITY** (env isolation)
- `TestParseAgentConfig_PathTraversalInResumeSession` — **SECURITY** (path validation scope)
- `TestNewBackend_ConfigFieldsThreaded` — validates config threaded to backend
- `TestHealthEndpoint_DoesNotExposeAgentConfig` — **SECURITY** (info disclosure)

**TRIM (12 tests — redundant or trivial):**
- `TestParseAgentConfig_EmptyObject_ReturnsDefaults` — Go JSON decoder default behavior
- `TestParseAgentConfig_UnsetEnv_ReturnsDefaults` — Go zero-value semantics
- `TestParseAgentConfig_UnknownFields_Ignored` — Go JSON unmarshal default behavior
- `TestParseAgentConfig_ExtremelyLongModel_DoesNotCrash` — trivial; no parsing logic special-cased for length
- `TestParseAgentConfig_NullAndMissingFields_UseDefaults` — Go JSON null handling (covered by zero-value test)
- `TestParseAgentConfig_WrongTypes_ReturnsError` — covered by `InvalidJSON` test; JSON unmarshaler always errors on type mismatch
- `TestParseAgentConfig_MaxTurnsNegative` — no validation in parseAgentConfig; validation is caller's job
- `TestParseAgentConfig_DeeplyNestedUnknownFields` — Go JSON decoder behavior; not relevant to application
- `TestParseAgentConfig_EmptyString_ReturnsDefaults` — duplicate of `UnsetEnv`
- `TestParseAgentConfig_WhitespaceOnly_ReturnsError` — edge case of JSON parsing; not agent-specific
- `TestNewBackend_InvalidConfig_FallsBackToDefaults` — test of defaults; covered by config-threaded test
- `TestNewBackend_ConfigPlusPrompt` — trivial getter/setter verification

**Recommendation:** Delete 12 trivial tests. Keep 6. Add 1 integration test: **"config from env var → backend → sidecar responds to health"** to validate end-to-end threading.

---

#### claude_test.go (5 tests) — KEEP ALL

**Status:** ✅ All tests are integration-focused.
- `TestClaudeBackend_SpawnWithDualChannel` — validates spawn with env, args, MCP server state
- `TestClaudeBackend_SendPrompt` — validates stdin message format
- `TestClaudeBackend_EventMapping` — **CRITICAL**: validates full event normalization (agent → text/tool/result/progress)
- `TestClaudeBackend_AutoApproval` — validates control flow for tool approval
- `TestClaudeBackend_ContextInjection` — validates context MCP protocol

---

#### codex_test.go (5 tests) — KEEP ALL

**Status:** ✅ All tests are integration-focused, testing protocol-level behavior.
- `TestCodexBackend_InitializeHandshake` — protocol validation (JSON-RPC handshake)
- `TestCodexBackend_SendPrompt_CreatesThread` — protocol validation (thread/turn lifecycle)
- `TestCodexBackend_SendSteer` — interactive steering validation
- `TestCodexBackend_EventMapping` — **CRITICAL**: event normalization (agent_message, tool_use, tool_result, etc.)
- `TestCodexBackend_AutoApproval` — tool approval flow

---

#### Other cmd/sidecar Tests

- **stall_test.go (8 tests):** All integration; validates stall detection timing logic. ✅ KEEP
- **main_test.go (3 tests):** Agent detection (claude vs codex). ✅ KEEP
- **ws_test.go (partial sample):** WebSocket protocol testing. ✅ KEEP
- **adversarial_test.go (12 tests):** Robustness testing (oversized payloads, injection). ✅ KEEP
- **fuzz_test.go:** Fuzzing. ✅ KEEP
- **mcp_test.go:** MCP protocol. ✅ KEEP
- **test_helpers_test.go:** Helper validation. Check if necessary.

**cmd/sidecar Summary:**
- DELETE: ~12 tests from agentconfig_test.go
- Total useful: ~36 tests

---

### pkg/api (52 tests) — STRONG, ONE FILE TO REVIEW

#### api_test.go (30 tests) — EXCELLENT INTEGRATION FOCUS

All tests are **critical path** integration tests exercising HTTP API → runtime → bridge → WebSocket:
- Session creation/deletion
- WebSocket lifecycle (connected → stdout → exit)
- Session state transitions
- Concurrent sessions
- Replay on reconnect
- Steering (stdin routing)
- Concurrent operations

**KEEP ALL.** These are the crown jewels of the suite.

---

#### api_adversarial_test.go (12 tests) — MOSTLY GOOD, SOME TRIM

**KEEP (8 tests — security/boundary validation):**
- `TestAdversarialCreateSession_UnknownAgent` — required validation
- `TestAdversarialCreateSession_UnknownRuntime` — required validation
- `TestAdversarialCreateSession_LongPromptHandled` — robustness
- `TestAdversarialCreateSession_NonexistentMountHost` — **SECURITY** (mount validation)
- Path traversal, symlink, permission tests — **SECURITY**

**TRIM (4 tests — low signal):**
- `TestAdversarialCreateSession_InvalidJSON` — duplicate of api_test.go handling; JSON validation is stdlib
- `TestAdversarialCreateSession_EmptyPrompt` — application accepts empty prompts (shell can run with no input)
- `TestAdversarialCreateSession_NullFields` — edge case; covered by schema validation tests
- `TestAdversarialCreateSession_ContentType` — HTTP header parsing (stdlib concern)

**Recommendation:** Delete 4 trivial tests.

---

#### Other api Tests

- **schema/types_test.go (2 tests):** Struct marshaling. Low value; could be trim candidates if not covering backward-compat.
- **session_lifecycle_test.go (5 tests):** Session state machine. ✅ KEEP
- **session_history_test.go (2 tests):** Log retrieval. ✅ KEEP
- **dashboard_test.go (1 test):** Dashboard endpoint. ✅ KEEP
- **usecase_test.go:** Feature-level scenarios. ✅ KEEP
- **fuzz_test.go:** Fuzzing. ✅ KEEP

**pkg/api Summary:**
- DELETE: ~4 tests from api_adversarial_test.go
- Total useful: ~48 tests

---

### pkg/runtime (97 tests) — EXCELLENT BUT BLOATED

#### docker_test.go (21 tests) — KEEP ALL

**Critical path:** Docker spawn config → security flags → mounts → env vars → WS port mapping.
- Tests validate security posture (--cap-drop, --init, no-new-privileges)
- Tests validate env materialization (AGENT_CMD, AGENT_PROMPT, proxy config)
- Tests validate volume mounts and resource limits
- Tests validate network setup

All are **high-value integration checks**. ✅ KEEP ALL.

---

#### env_isolation_test.go (16 tests) — STRONG BUT OPPORTUNITIES

**Assessment:** Tests environment variable isolation and stripping. Some tests are granular unit tests of helper functions.

**KEEP (10 tests — critical for security):**
- Integration tests validating that secrets don't leak into container
- Tests validating HOME stripping, PATH isolation, cred stripping
- Concurrent access tests

**TRIM (6 tests — trivial helpers):**
- Tests that just validate string parsing functions (e.g., `TestParseEnvLine_SimpleKey`)
- Tests of internal helper functions that are thin wrappers
- Recommendation: Merge trivial edge cases into fewer integration tests; delete the "unit test" style helpers

---

#### docker_adversarial_test.go (11 tests) — GOOD ROBUSTNESS

- Tests large image names, long session IDs, malformed config
- Tests resource limit edge cases
- All tests add security/robustness value. ✅ KEEP ALL.

---

#### Other runtime Tests

- **local_test.go (9 tests):** Local runtime spawn validation. ✅ KEEP
- **network_test.go (3 tests):** Network bridge validation. ✅ KEEP
- **network_iptables_test.go (2 tests):** iptables setup. ✅ KEEP (security-critical)
- **resource_test.go (9 tests):** Resource limit parsing. Some trim opportunities here.
- **wshandle_test.go (6 tests):** WebSocket handle management. ✅ KEEP
- **agentconfig_test.go (6 tests):** Config serialization. Similar to cmd/sidecar; trim candidates.

**pkg/runtime Summary:**
- DELETE: ~6 tests from env_isolation_test.go (trivial helpers)
- DELETE: ~6 tests from agentconfig_test.go (trivial serialization)
- Total useful: ~85 tests

---

### pkg/session (103 tests) — COMPREHENSIVE BUT INCLUDES TRIVIAL GETTERS

#### session_test.go (32 tests) — MIXED

**KEEP (18 tests — critical state machine logic):**
- Session lifecycle: Pending → Running → Completed/Failed/Orphaned
- Concurrent state transitions
- Concurrency stress tests (ConcurrentAddGet, ConcurrentRemove, ConcurrentSetCompleted)
- Manager CRUD (Add, Get, Remove, List)
- Max sessions enforcement
- Replay buffer initialization
- Recovery (orphan handling)
- Metrics recording (token counting, tool call counting)

**TRIM (14 tests — trivial assertions on trivial getters):**
- `TestNewSession_InitialState` — constructor validation; trivial
- `TestSession_IDUnique` — RNG validation; covered by deployment testing
- `TestSession_SetRunning` — trivial state setter
- `TestSession_SetCompleted_ZeroExit` — covered by lifecycle tests
- `TestSession_SetCompleted_NonZeroExit` — covered by lifecycle tests
- `TestSession_SetCompleted_KillCode` — covered by lifecycle tests
- `TestGetSession_Found` — thin wrapper around manager
- `TestManager_GetMissing` — trivial
- `TestManager_RemoveMissing` — trivial (must not panic)
- `TestStateConstants` — validates string constant values; covered by API contract tests
- `TestSession_EndedAt_SetOnCompletion` — timing assertion; low value
- `TestSession_RecordActivity` — metric recording; low value
- Tests for simple metric accumulation (RecordUsage, RecordToolCall) — covered by concurrent tests

**Recommendation:** Delete 14 trivial tests. Keep 18 critical tests.

---

#### replay_test.go (17 tests) — KEEP ALL

**Critical for replay-on-reconnect:** Offset tracking, circular buffer wraparound, encoding, time-based expiration.
All tests are **high-value**. ✅ KEEP ALL.

---

#### logfile_test.go (11 tests) — KEEP ALL

Tests NDJSON log formatting, parsing, session ID extraction. Critical for recovery path. ✅ KEEP ALL.

---

#### logreader_test.go (8 tests) — KEEP ALL

Tests log file reading, cursor advancement, offset tracking. Critical for recovery. ✅ KEEP ALL.

---

#### fault_injection_test.go (10 tests) — KEEP ALL

Validates session handles Chaos engineering scenarios (simulated crash, reconnect, orphan). ✅ KEEP ALL.

---

#### agentsessions Tests

- **claude_test.go (10 tests):** Claude session persistence/recovery. ✅ KEEP
- **prune_test.go (3 tests):** Old session cleanup. ✅ KEEP

---

#### validate_test.go (6 tests) — MOSTLY KEEP, MINOR TRIM

Tests path validation (no traversal, absolute paths). One or two trivial edge cases could be merged.

**KEEP:** Path security tests. **MINOR TRIM:** Remove redundant "empty string" edge case tests.

---

**pkg/session Summary:**
- DELETE: ~14 tests from session_test.go (trivial getters/setters)
- Total useful: ~89 tests

---

### pkg/bridge (18 tests) — MIXED

#### bridge_test.go (4 tests) — TRIM CANDIDATES

- `TestServerFrame_JSONRoundtrip` — JSON marshaling (stdlib functionality)
- `TestClientFrame_JSONRoundtrip` — JSON marshaling (stdlib functionality)
- `TestServerFrame_StdoutType` — string-search in JSON (trivial)
- `TestServerFrame_ReplayType` — string-search in JSON (trivial)

**TRIM ALL 4.** These test struct marshaling, which is stdlib and unlikely to break. Integration tests (api_test.go) already validate frame serialization end-to-end.

**Recommendation:** Delete 4 tests. Replace with 1 integration test in api_test.go: "frame sent over WS is valid JSON."

#### bridge_integration_test.go (6 tests) — KEEP ALL

Tests actual bridge forwarding logic (stdin routing, output broadcast, replay). ✅ CRITICAL.

#### bridge_steerable_test.go (3 tests) — KEEP ALL

Tests interactive steering (sending stdin mid-session). ✅ CRITICAL.

#### bridge_adversarial_test.go (2 tests) — KEEP ALL

Robustness tests (large payloads, invalid frames). ✅ KEEP.

#### fuzz_test.go — KEEP

---

**pkg/bridge Summary:**
- DELETE: 4 tests from bridge_test.go (trivial JSON marshaling)
- Total useful: ~14 tests

---

### pkg/errors (12 tests) — EXCELLENT

#### classify_test.go (12 tests)

**All tests are high-value:**
- `TestClassify` — error pattern recognition (auth, rate limit, model not found, etc.)
- `TestRetryable` — determines which errors are retriable
- `TestDetectStartupCrash` — heuristic for spawn failure (0 tokens + small output + 2KB threshold)
- `TestClassifyFromEvents` — prevents false-positive error detection (ignores tool_result fields with "error" key)

**KEEP ALL 12.** These are **critical for reliability**: misclassification breaks retry logic.

---

### pkg/materialize (33 tests) — HEALTHY

#### materializer_test.go (12 tests) — KEEP ALL

Tests credential discovery, MCP server config, env materialization. All integration-focused. ✅ KEEP.

#### credential_discovery_test.go (3 tests) — KEEP ALL

#### discover_test.go (5 tests) — KEEP ALL

#### gateway_test.go (3 tests) — KEEP ALL

#### adversarial_test.go (5 tests) — KEEP ALL

#### fuzz_test.go — KEEP

---

### pkg/agent (42 tests) — TRIM OPPORTUNITIES

#### agent_test.go (3 tests) — KEEP

#### parse_test.go (5 tests) — LIKELY TRIM

Tests of simple string parsing. If these are helper function unit tests for internal functions, they add little value (parse is invoked end-to-end by api/session tests).

**Action:** Review and likely delete unless testing critical parsing logic.

#### parse_adversarial_test.go (3 tests) — KEEP

Robustness (large input, malformed, injection). ✅ KEEP.

#### claude_test.go (8 tests) — KEEP

Agent command building. ✅ KEEP.

#### codex_test.go (8 tests) — KEEP

Agent command building. ✅ KEEP.

#### injection_test.go (7 tests) — KEEP

Path injection detection. ✅ SECURITY.

#### fuzz_test.go — KEEP

---

### pkg/credentials (11 tests)

#### codex_refresh_test.go (5 tests) — KEEP ALL

Codex token refresh logic. ✅ KEEP.

#### sync_test.go (6 tests) — KEEP ALL

Credential synchronization. ✅ KEEP.

---

### pkg/client (8 tests)

#### client_test.go — KEEP ALL

Client SDK tests. ✅ KEEP.

#### adversarial_test.go — KEEP ALL

Client robustness. ✅ KEEP.

#### fuzz_test.go — KEEP

---

### pkg/e2e (18 tests) — EXCELLENT

#### e2e_test.go (12 tests) — EXCELLENT

Full end-to-end tests (daemon → runtime → sidecar → agent → events → client).
✅ **KEEP ALL.** These are the ultimate integration tests.

#### sidecar_v2_test.go (4 tests) — KEEP ALL

V2 sidecar protocol. ✅ KEEP.

#### concurrency_test.go (2 tests) — KEEP ALL

Parallel session handling. ✅ KEEP.

---

## Summary Table

| Package | Tests | KEEP | TRIM | Missing | Notes |
|---------|-------|------|------|---------|-------|
| cmd/agentd | 3 | 3 | — | — | ✅ Lean |
| cmd/sidecar | 48 | 36 | 12 | 1 | Trim agentconfig trivial tests; add 1 integration test |
| pkg/api | 52 | 48 | 4 | — | Trim api_adversarial trivial tests |
| pkg/runtime | 97 | 85 | 12 | — | Trim env_isolation helpers, agentconfig trivial |
| pkg/session | 103 | 89 | 14 | — | Trim trivial getters/setters |
| pkg/bridge | 18 | 14 | 4 | — | Delete struct marshaling tests |
| pkg/errors | 12 | 12 | — | — | ✅ Excellent |
| pkg/materialize | 33 | 33 | — | — | ✅ All valuable |
| pkg/agent | 42 | 37 | 5 | — | Likely trim parse_test.go helpers |
| pkg/credentials | 11 | 11 | — | — | ✅ Keep all |
| pkg/client | 8 | 8 | — | — | ✅ Keep all |
| pkg/e2e | 18 | 18 | — | — | ✅ Excellent |
| **TOTAL** | **576** | **544** | **32** | **1** | **Net: 513 truly valuable tests** |

---

## Recommended Actions (Priority Order)

### Priority 1: DELETE (Low-hanging fruit)

1. **cmd/sidecar/agentconfig_test.go** → Delete 12 tests:
   - `TestParseAgentConfig_EmptyObject_ReturnsDefaults`
   - `TestParseAgentConfig_UnsetEnv_ReturnsDefaults`
   - `TestParseAgentConfig_UnknownFields_Ignored`
   - `TestParseAgentConfig_ExtremelyLongModel_DoesNotCrash`
   - `TestParseAgentConfig_NullAndMissingFields_UseDefaults`
   - `TestParseAgentConfig_WrongTypes_ReturnsError`
   - `TestParseAgentConfig_MaxTurnsNegative`
   - `TestParseAgentConfig_DeeplyNestedUnknownFields`
   - `TestParseAgentConfig_EmptyString_ReturnsDefaults`
   - `TestParseAgentConfig_WhitespaceOnly_ReturnsError`
   - `TestNewBackend_InvalidConfig_FallsBackToDefaults`
   - `TestNewBackend_ConfigPlusPrompt`

2. **pkg/api/api_adversarial_test.go** → Delete 4 tests:
   - `TestAdversarialCreateSession_InvalidJSON`
   - `TestAdversarialCreateSession_EmptyPrompt`
   - `TestAdversarialCreateSession_NullFields`
   - `TestAdversarialCreateSession_ContentType`

3. **pkg/bridge/bridge_test.go** → Delete 4 tests:
   - `TestServerFrame_JSONRoundtrip`
   - `TestClientFrame_JSONRoundtrip`
   - `TestServerFrame_StdoutType`
   - `TestServerFrame_ReplayType`

4. **pkg/session/session_test.go** → Delete 14 tests:
   - `TestNewSession_InitialState`
   - `TestSession_IDUnique`
   - `TestSession_SetRunning`
   - `TestSession_SetCompleted_ZeroExit`
   - `TestSession_SetCompleted_NonZeroExit`
   - `TestSession_SetCompleted_KillCode`
   - `TestManager_GetMissing`
   - `TestManager_RemoveMissing`
   - `TestStateConstants`
   - `TestSession_EndedAt_SetOnCompletion`
   - `TestSession_RecordActivity`
   - `TestSession_RecordUsage`
   - `TestSession_RecordToolCall`
   - `TestSession_Snapshot_IncludesMetrics`

5. **pkg/runtime** → Delete ~12 tests:
   - env_isolation_test.go: `TestParseEnvLine_*` (trivial helpers)
   - agentconfig_test.go: Similar to cmd/sidecar; delete trivial serialization tests

**Subtotal: ~46 tests to delete**

### Priority 2: REVIEW & LIKELY DELETE

6. **pkg/agent/parse_test.go** → Review 5 tests
   - If these are unit tests of internal helper functions, delete them
   - If they test public API boundary, keep them

7. **pkg/runtime/resource_test.go** → Review 9 tests
   - Trim trivial resource limit parsing tests (keep integration validation)

### Priority 3: ADD (New test coverage gaps)

8. **Add 1 integration test in cmd/sidecar:**
   - `TestAgentConfig_EndToEnd_EnvVarToBackendToHealth`
   - Validates config from env var → parsed → threaded to backend → health endpoint reports it

9. **Add integration test for path traversal prevention:**
   - Ensure ResumeSession path validation happens at API boundary (not just parseAgentConfig)

10. **Consider adding stall detection false-positive test:**
    - Verify that sessions with legitimate long silence aren't incorrectly classified as stalled

---

## Testing Philosophy Alignment

### Current State
- ✅ **Integration tests dominate:** api_test.go, e2e_test.go, bridge integration tests
- ✅ **Security tests strong:** path validation, env isolation, shell injection prevention
- ✅ **Concurrency tested:** manager concurrent ops, session metrics concurrent recording
- ✅ **Error handling:** classification logic, retry policy

### Bloat Areas
- ❌ **Struct marshaling as tests:** bridge_test.go (trivial JSON round-trip)
- ❌ **Trivial getter/setter tests:** session_test.go state constants, properties
- ❌ **Internal helper function tests:** parse_test.go, env_isolation helpers (not public API)
- ❌ **Type system validation:** JSON null handling, zero-value tests (Go compiler guarantees these)

### Rebalancing Opportunity
After deletions, **shift focus to**:
1. Protocol correctness: Claude/Codex message format (already strong)
2. Error classification: Prevent false positives in retry logic (already strong)
3. Recovery paths: Orphan session recovery, log replay accuracy (already strong)
4. Concurrency: Session manager stress under load (good coverage)
5. **Gap:** Daemon lifecycle under failure (process crash, recovery, shutdown-all)

---

## Risk Assessment

**Low Risk:** Deleting tests identified as "trim." These validate:
- Go stdlib behavior (JSON, marshal, type system)
- Internal implementation details (not part of public API)
- Trivial property assignment

**No Risk:** Tests for type-level guarantees (Go compiler already enforces these).

**Safety:** All deletions are in unit test categories. Integration tests (api_test.go, e2e_test.go, bridge_integration) are KEPT intact and provide safety net.

---

## Expected Outcome

After recommended changes:
- **~46–50 tests deleted** (bloat removal)
- **1–2 tests added** (critical path coverage)
- **~530 meaningful tests remain**
- **Test suite complexity reduced** by ~8%
- **Test maintenance burden** reduced (fewer trivial edge case assertions)
- **CI time** slightly reduced
- **Signal-to-noise ratio** improved (ratio of valuable tests to total tests increases)

Test suite remains **heavily integration-focused** with strong coverage of:
- Session lifecycle (create → run → complete)
- WebSocket protocol (frame types, reconnect, replay)
- Docker runtime security (cap drop, init, mounts, env)
- Error classification (prevents misdiagnosis)
- Concurrency (thread safety of session manager)
- Recovery (orphaned sessions, log replay)

---

## Notes for Implementation

1. Run `go test -v ./...` after each deletion batch to ensure no unexpected test interdependencies
2. Verify CI passes with deletions before committing
3. Consider marking tests with `// DEPRECATED` comment before deletion (gives team visibility)
4. If any deleted test catches a regression later, restore it and add integration test covering the scenario
