# Native stream contract fixtures

These scrubbed fixtures preserve the provider-facing JSON shapes currently
verified by the Claude stream-json and Codex app-server adapters. They contain
no prompts, credentials, host paths, or provider-generated content from a real
session.

Each file is NDJSON. Records must be retained exactly as read; normalization is
a derived view and must not replace these bytes.

Coverage:

- Claude input: prompt, interrupt, and tool approval response.
- Claude output: lifecycle, content delta, tool call, tool result, usage,
  permission request, error, and terminal result.
- Codex input: handshake, thread start/resume, turn start/steer/interrupt, and
  approval response.
- Codex output: handshake response, lifecycle, content delta, tool call/result,
  usage, approval request, error, and terminal turn completion.

Live paid-provider probes remain opt-in. The fixtures are the deterministic CI
contract; live probes are compatibility checks, not unit-test dependencies.
