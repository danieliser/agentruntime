# AGENTS.md — Project Contract

Every agent working in this codebase follows these principles. No exceptions, no shortcuts.

---

## Agent Directives

How agents think, communicate, and work — before writing a single line of code.

**Token efficiency.** Say what needs to be said, nothing more. No preamble, no restating the question, no trailing summaries of what you just did. Every token should earn its place.

**Scannable responses.** Lead with the answer or the key decision. Use formatting to surface structure — headers, bold for critical points, tables for comparisons, `---` dividers between distinct sections. Long prose buries insight. Short, structured output respects the reader's time.

**Read before writing.** Before implementing any solution, scan the codebase for existing helpers, patterns, and utilities. If Tessera MCP is available, prefer it for semantic search, symbol lookup, and cross-reference tracing — it's faster and more precise than grep across large codebases. Fall back to LSP (symbols, references, go-to-definition), then grep. The best code is often the code you don't have to write.

**Troubleshoot effectively.** Evidence first, hypothesis second, fix third. Read the actual error. Reproduce the failure before touching code. Identify root cause — not symptoms. If 3+ attempted fixes fail, stop and reassess the entire mental model. Never claim something is fixed without running it and reading the output.

---

## 1. Test-Driven Development

**TDD is mandatory.** All new features, bug fixes, and refactors follow Red-Green-Refactor:

1. **Red** — Write a failing test that defines the expected behavior
2. **Green** — Write the minimal code to make it pass
3. **Refactor** — Clean up without changing behavior, tests stay green

- Watch the test fail first. A test that has never failed has never proven anything.
- Tests must be fast and deterministic. Mock external dependencies (DB, API, filesystem, network).
- No implementation is "done" until its tests pass. Run them. Read the output. No "should work."
- If fixing a bug, the test must reproduce the bug before you write the fix.

---

## 2. DRY — Don't Repeat Yourself

- Extract shared logic into helpers/modules. Three similar blocks = time to abstract.
- But don't over-abstract. A premature abstraction is worse than duplication. If you can't name the abstraction clearly, it's not ready.
- When you find duplication during a refactor, fix it. Don't leave it for later.

---

## 3. Single Point of Usage

Every API, function, feature, and integration should have **one canonical entry point**.

- No parallel implementations of the same thing. If two modules do the same job, one goes.
- Shared logic lives in a dedicated module, imported by consumers — not copy-pasted.
- Every public interface should be isolatable for testing without standing up the full system.
- Thin surface files or index modules re-export the public API. Consumers import from one place.

---

## 4. Configuration

All user-configurable values follow this hierarchy:

1. **Environment variables** (highest priority, runtime override)
2. **Config files** (`.env`, YAML, TOML — committed or gitignored as appropriate)
3. **System defaults** (hardcoded, reasonable, documented)

Rules:

- Never hardcode secrets, API keys, or environment-specific values.
- Every config option must have a sensible default so the system runs without manual setup.
- Maintain a `*.example` file (e.g., `.env.example`, `config.example.yaml`) documenting every option with descriptions and default values.
- Config loading happens once at startup, in one place. No scattered `os.getenv()` calls deep in business logic.

---

## 5. Security

- Follow OWASP top 10. No command injection, SQL injection, XSS, SSRF, or path traversal.
- Validate at system boundaries (user input, external APIs, agent output). Trust internal code.
- Secrets are never logged, never committed, never included in agent output. Scrubbing is your safety net, not your strategy.
- Principle of least privilege: agents and services get the minimum permissions needed for their task.
- All new attack vectors discovered during development get a test.

---

## 6. Code Quality & Formatting

- **500-line soft cap per file.** Approaching it? Split. No monoliths.
- **One tool/handler per file** for extensible systems (MCP tools, API handlers, CLI commands). Enables dynamic discovery and individual addressability.
- Type all function signatures. Avoid `any` (`interface{}`) except where genuinely required by the API contract.
- Structs for structured data crossing package boundaries. No `map[string]interface{}` as public API.
- Linting and formatting rules are enforced by tooling, not by memory. Follow whatever the project's linter/formatter dictates.

---

## 7. Error Handling

- Structured errors with codes, not string matching. Errors may be consumed by other agents or machines.
- Project-level error hierarchy: domain-specific errors inheriting from a common base.
- Fail loudly at boundaries, handle gracefully internally. Don't swallow errors silently.
- Log errors with context (what was attempted, what failed, correlation ID if available).
- No bare `recover()` without re-panicking or logging, no bare error discards (`_ = err`) without re-raising or explicit justification.

---

## 8. Async & Concurrency Discipline

- No fire-and-forget goroutines without a WaitGroup, errgroup, or supervision context.
- Use `defer` for resource cleanup (DB connections, HTTP sessions, file handles).
- Cancellation-safe code: if a task can be cancelled, clean up properly.
- Connection pooling over per-request connections.
- Timeouts on all external calls. No unbounded waits.

---

## 9. API Contract Stability

- Versioned endpoints (`/api/v1/...`). Breaking changes = new version.
- Consistent response envelope across all endpoints.
- Backwards-compatible changes only within a version (additive fields OK, removals are breaking).
- Document API surfaces. If an agent or external consumer depends on it, it's a contract.

---

## 10. Graceful Degradation

- Optional components can fail without taking down the core.
- No hard dependency on any single integration, delivery channel, or runtime.
- Feature-detect at startup, degrade at runtime. Log what's unavailable, don't crash.
- Health checks should distinguish "healthy" from "degraded" from "down."

---

## 11. Idempotency

- Dispatch, delivery, and migration operations must be safe to retry.
- Use dedup keys where needed (content hashes, unique constraints, idempotency tokens).
- Database operations: prefer upsert patterns over insert-and-hope.
- If an operation has side effects, make them idempotent or guard with state checks.

---

## 12. Interface Contracts

- Abstract types (interfaces, ABCs, Protocols) for every extension point.
- New implementations conform to the interface, not the other way around.
- Interfaces are tested independently via contract tests — if you implement a contract, there's a test suite that validates any implementation.

---

## 13. Plugin-First Architecture

- New integrations should be designed as plugins from day one.
- Tightly-coupled integrations are candidates for extraction when they stabilize.
- Plugins interact through defined interfaces (see #12), not by reaching into core internals.
- Core provides hooks, registries, and lifecycle management. Plugins register themselves.
- A broken or missing plugin never crashes the host.

---

## 14. Infrastructure Extraction

- Backbone systems (runtimes, executors, session management, protocol layers) should be extractable into standalone, independently testable packages.
- Design module boundaries as if they'll become separate repos. Clean imports, no circular dependencies, explicit public APIs.
- This doesn't mean extract now — it means structure so extraction is a mechanical task, not a rewrite.

---

## 15. Immutable Data & Data Integrity

- **Prefer immutable data structures** where practical. Once created, data should not be mutated in place — create new instances instead. This eliminates concurrency bugs and makes state changes traceable.
- **Audit records, logs, and signed payloads are append-only.** Never update or delete audit trail entries. Immutability enforced by database triggers, not application trust.
- **Session state should be recoverable.** Backups, snapshots, and checkpoints enable resume-after-failure without data loss.
- **Log segmentation and archival.** Logs rotate by size or time, archived to durable storage. Old logs are compressed and retained, not deleted.
- **Database backups are automated and tested.** A backup you've never restored from is not a backup.
- **Signed or hashed outputs are tamper-evident.** Ed25519 signatures on task results ensure integrity from agent output to storage. Verify on read when trust matters.

---

## 16. Observability

- Structured logging. No bare `fmt.Println()` in production code — use the structured logger.
- Correlation IDs across task lifecycle (request/session ID is your trace ID..
- Audit trail for all state-changing operations (the audit logger).
- Secrets scrubbed before any logging or storage.
- Metrics and health endpoints for operational monitoring.

---

## 17. Dependency Management

- Minimal transitive dependencies. Every new dependency needs justification.
- Pin versions in `go.sum` / `go.mod`. No floating ranges in production.
- Audit dependencies periodically. If something isn't used, remove it.
- Prefer stdlib solutions over third-party packages for simple tasks.

---

## 18. Database & Migration Discipline

- Migrations are irreversible production changes. **Never create migration files without planning and review.**
- Never use `DROP TABLE` — use rename-copy-drop pattern.
- Sequential SQL files in `migrations/`, applied by the schema manager.
- No raw SQL scattered in business logic — queries go through the database layer.
- Schema changes require discussion, review, and a plan. No exceptions.

---

## Summary

These principles exist to keep the codebase maintainable, secure, and extensible as it grows. They're not aspirational — they're the bar. If a principle conflicts with shipping, raise it. Don't silently skip it.
