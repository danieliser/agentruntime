# Config Shape Design Analysis

**Date:** 2026-03-16
**Status:** Design review — stress-testing the v0.2.0 `SessionRequest` shape
**Scope:** The canonical config struct that serves HTTP JSON, Go SDK, and YAML file dispatch equally

---

## 1. Verdict on Current Shape

### What's Right

**Agent-namespaced sibling keys (`claude:`, `codex:`) — correct.** This is the Nomad pattern.
Nomad's `config {}` block is namespaced by driver (`docker {}`, `exec {}`, `raw_exec {}`), and
it works well for exactly the same reason: driver configs are structurally incompatible, a
discriminated union would force artificial commonality, and the "only the matching block is read"
rule is trivially enforceable. The agentruntime agents have zero config overlap — Claude has
`settings_json` and `claude_md`, Codex has `config_toml` and `approval_mode`. Forcing these
into a shared `agent_config:` with a `type:` discriminator adds a level of indirection that
buys nothing and makes the YAML harder to read.

**Flat top-level with `agent` + `prompt` + `work_dir` as required trinity — correct.** This
mirrors GitHub Actions' `runs-on` + `steps` simplicity. The minimal request is three fields.
Everything else has sane defaults. This is the right call.

**`work_dir` as shorthand that expands to a Mount — correct.** Docker Compose does this with
`volumes:` short syntax. It eliminates the "I just want to point at a directory" ceremony while
keeping the full `mounts:` available for power users.

**`env:` as clean-room (not inherited) — correct.** Kubernetes does this. GitHub Actions does
this. Never inheriting host env is the only defensible default for a multi-tenant runtime.

**`mcp_servers` at top level — correct.** MCP servers are infrastructure, not agent config.
They describe *what services exist*, and agentruntime materializes them into whatever format
the active agent expects (`.mcp.json` for Claude, env vars or flags for others). If they lived
inside `claude:`, you'd duplicate them when switching agents. Top-level is right.

**`resources:` as an optional block — correct.** This maps cleanly to Kubernetes
`resources.limits` and Docker `--memory`/`--cpus`. Optional with sane defaults.

### What's Wrong

**`pty` is in `Resources` — wrong.** PTY is session behavior, not a resource constraint. It
affects stdio multiplexing (stderr merges into stdout), bridge framing, and replay buffer
semantics. It belongs at the top level or in a future `session:` block, not next to `memory`
and `cpus`. Kubernetes puts `tty: true` on the container spec, not in resource limits, for
exactly this reason.

**`image` is in `Resources` — debatable but wrong.** The container image is the execution
environment identity, not a resource constraint. `memory: 4g` is a limit. `image: ubuntu:22.04`
is "what OS am I running in." Docker Compose puts `image:` at the service level, not inside
`deploy.resources`. It should be in `Resources` only if we rename it to something broader
(like `container:` or `runtime_config:`), or it should be top-level/in a runtime config block.

**No `model` field at the top level.** The current `AgentConfig` struct has `Model`, and it's
a cross-agent concept (Claude has models, Codex has models, OpenCode has models). It should be
a top-level convenience field, with the agent-specific config able to override.

**`timeout_ms` uses milliseconds in the field name.** This is a YAML-file-on-disk format. Humans
don't think in milliseconds. Go's `time.Duration` would be ideal in the Go struct, but for YAML/JSON
a string like `"5m"` or `"300s"` would be far more humane. Internally parse to ms. The field name
should be `timeout` with string parsing, not `timeout_ms` with raw integers.

### What's Missing

**No `model` at top level** (noted above).

**No `session_id` for resume.** The current `AgentConfig` has `SessionID` for session resume,
but `SessionRequest` doesn't expose it. If you want to resume a Claude session or reconnect
to an existing Codex conversation, there's no way to express that.

**No `name` or `description`.** For human-authored YAML files and observability, a `name` field
(like Docker Compose service names or GitHub Actions job names) is valuable. `task_id` is a UUID
for machines. `name` is "fix-auth-bug" for humans.

---

## 2. Alternative Designs

### Alternative A: Current Shape, Refined

The current shape is ~90% right. This alternative fixes the identified issues without restructuring.

```yaml
# Minimal
agent: claude
prompt: "Fix the failing tests"
work_dir: /path/to/project

# Full
name: fix-auth-tests                    # human label (optional)
task_id: "uuid"                         # machine ID (optional, auto-generated)
agent: claude
runtime: docker
prompt: "Fix the failing tests"
model: claude-sonnet-4-6                # top-level convenience
timeout: 5m                             # human-readable duration
resume_session: "session-uuid"          # reconnect to prior session

work_dir: /path/to/project
mounts:
  - host: /shared-libs
    container: /deps
    mode: ro

claude:
  settings_json:
    permissions:
      deny: ["Bash", "Bash(*)"]
  claude_md: |
    # Instructions
    You are working on the agentruntime Go library.
  credentials_path: ~/.claude/credentials.json
  memory_path: ~/.claude/projects/agentruntime/
  output_format: stream-json

mcp_servers:
  - name: persist
    type: http
    url: http://${HOST_GATEWAY}:8801/mcp
    token: ${PERSIST_AUTH_TOKEN}

env:
  GITHUB_TOKEN: ghp_xxx

container:                               # renamed from "resources"
  image: ubuntu:22.04
  memory: 4g
  cpus: 2.0
  network: bridge
  security_opt:
    - no-new-privileges:true

pty: false                               # moved to top level
tags:
  project: agentruntime
```

```go
type SessionRequest struct {
    // Identity
    Name          string            `json:"name,omitempty"           yaml:"name,omitempty"`
    TaskID        string            `json:"task_id,omitempty"        yaml:"task_id,omitempty"`
    Tags          map[string]string `json:"tags,omitempty"           yaml:"tags,omitempty"`

    // What to run
    Agent         string `json:"agent"                    yaml:"agent"`
    Runtime       string `json:"runtime,omitempty"        yaml:"runtime,omitempty"`
    Prompt        string `json:"prompt"                   yaml:"prompt"`
    Model         string `json:"model,omitempty"          yaml:"model,omitempty"`
    Timeout       string `json:"timeout,omitempty"        yaml:"timeout,omitempty"` // "5m", "300s"
    ResumeSession string `json:"resume_session,omitempty" yaml:"resume_session,omitempty"`

    // Filesystem
    WorkDir string  `json:"work_dir,omitempty" yaml:"work_dir,omitempty"`
    Mounts  []Mount `json:"mounts,omitempty"   yaml:"mounts,omitempty"`

    // Agent-specific
    Claude *ClaudeConfig `json:"claude,omitempty" yaml:"claude,omitempty"`
    Codex  *CodexConfig  `json:"codex,omitempty"  yaml:"codex,omitempty"`

    // Infrastructure
    MCPServers []MCPServer       `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`
    Env        map[string]string `json:"env,omitempty"         yaml:"env,omitempty"`
    Container  *ContainerConfig  `json:"container,omitempty"   yaml:"container,omitempty"`
    PTY        bool              `json:"pty,omitempty"         yaml:"pty,omitempty"`
}

type ContainerConfig struct {
    Image       string   `json:"image,omitempty"        yaml:"image,omitempty"`
    Memory      string   `json:"memory,omitempty"       yaml:"memory,omitempty"`
    CPUs        float64  `json:"cpus,omitempty"         yaml:"cpus,omitempty"`
    Network     string   `json:"network,omitempty"      yaml:"network,omitempty"`
    SecurityOpt []string `json:"security_opt,omitempty" yaml:"security_opt,omitempty"`
}
```

**Key changes:** `pty` → top-level. `resources` → `container` (honest naming). Added `name`,
`model`, `timeout` (human duration), `resume_session`. Dropped `timeout_ms`.


### Alternative B: Grouped Task Block

Separates "what to do" (task) from "how to do it" (infrastructure). Inspired by
GitHub Actions' separation of `jobs.<id>` from `runs-on` + `env`.

```yaml
task:
  id: "uuid"
  name: fix-auth-tests
  prompt: "Fix the failing tests"
  timeout: 5m
  tags:
    project: agentruntime

agent: claude
model: claude-sonnet-4-6
runtime: docker

work_dir: /path/to/project
mounts:
  - host: /shared-libs
    container: /deps
    mode: ro

claude:
  settings_json:
    permissions:
      deny: ["Bash", "Bash(*)"]
  claude_md: |
    # Instructions
  credentials_path: ~/.claude/credentials.json

mcp_servers:
  - name: persist
    type: http
    url: http://${HOST_GATEWAY}:8801/mcp

env:
  GITHUB_TOKEN: ghp_xxx

container:
  image: ubuntu:22.04
  memory: 4g
  cpus: 2.0

pty: false
```

```go
type SessionRequest struct {
    Task      TaskSpec          `json:"task"                  yaml:"task"`
    Agent     string            `json:"agent"                 yaml:"agent"`
    Model     string            `json:"model,omitempty"       yaml:"model,omitempty"`
    Runtime   string            `json:"runtime,omitempty"     yaml:"runtime,omitempty"`

    WorkDir   string            `json:"work_dir,omitempty"    yaml:"work_dir,omitempty"`
    Mounts    []Mount           `json:"mounts,omitempty"      yaml:"mounts,omitempty"`

    Claude    *ClaudeConfig     `json:"claude,omitempty"      yaml:"claude,omitempty"`
    Codex     *CodexConfig      `json:"codex,omitempty"       yaml:"codex,omitempty"`

    MCPServers []MCPServer      `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`
    Env        map[string]string `json:"env,omitempty"        yaml:"env,omitempty"`
    Container  *ContainerConfig `json:"container,omitempty"   yaml:"container,omitempty"`
    PTY        bool             `json:"pty,omitempty"         yaml:"pty,omitempty"`
}

type TaskSpec struct {
    ID      string            `json:"id,omitempty"      yaml:"id,omitempty"`
    Name    string            `json:"name,omitempty"    yaml:"name,omitempty"`
    Prompt  string            `json:"prompt"            yaml:"prompt"`
    Timeout string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
    Tags    map[string]string `json:"tags,omitempty"    yaml:"tags,omitempty"`
}
```

**Key changes:** `prompt`, `task_id`, `name`, `timeout`, `tags` grouped into `task:`. Everything
else stays flat. Minimal request is `agent` + `task.prompt` + `work_dir`.

**Problem:** The minimal request now requires nesting: `task: { prompt: "..." }` instead of
just `prompt: "..."`. This adds friction to the simplest case for zero benefit. The grouping
is aesthetically clean but practically worse for the most common path.


### Alternative C: Discriminated Union for Agent Config

Uses `agent_config:` with an explicit `type:` field instead of sibling keys.
Inspired by Kubernetes' `spec.containers[].resources` pattern.

```yaml
agent: claude
prompt: "Fix the failing tests"
work_dir: /path/to/project

agent_config:
  type: claude                          # redundant with top-level `agent:`
  settings_json:
    permissions:
      deny: ["Bash", "Bash(*)"]
  claude_md: |
    # Instructions
  credentials_path: ~/.claude/credentials.json

# ... rest same as Alternative A
```

```go
type SessionRequest struct {
    // ... same top-level fields as Alt A ...
    AgentConfig *AgentSpecificConfig `json:"agent_config,omitempty" yaml:"agent_config,omitempty"`
}

// AgentSpecificConfig uses a type discriminator to select the config shape.
// In Go, this requires custom JSON/YAML unmarshalling.
type AgentSpecificConfig struct {
    Type string `json:"type" yaml:"type"`

    // Only one of these is populated based on Type.
    // In Go, we'd use a custom UnmarshalJSON/UnmarshalYAML.
    Claude *ClaudeConfig `json:"-" yaml:"-"`
    Codex  *CodexConfig  `json:"-" yaml:"-"`
}
```

**Problems:**

1. **`type: claude` is redundant with `agent: claude`.** You already declared the agent. Now
   you're declaring it again inside agent_config. If they disagree, which wins?

2. **Custom unmarshalling is required.** The sibling-key pattern (`claude:`, `codex:`) works
   with standard `json.Unmarshal` and `yaml.Unmarshal` — no custom logic. The discriminated
   union requires writing `UnmarshalJSON` and `UnmarshalYAML` methods that inspect `type` and
   then dispatch to the right struct. This is real complexity for no gain.

3. **YAML authoring is worse.** With sibling keys, you write `claude:` and your editor/schema
   gives you the right completions. With `agent_config:`, you write a generic block and the
   schema can't know which fields are valid until it reads `type:`.

4. **Nomad tried both patterns and settled on the sibling approach** — the `config {}` block
   inside `task {}` is implicitly typed by the `driver` field. They don't have a `type:` inside
   `config {}`.

---

## 3. Scoring Table

| Criterion                          | A: Refined Flat | B: Grouped Task | C: Discriminated Union |
|------------------------------------|:-:|:-:|:-:|
| **Intuitiveness** (can a human write it from memory?) | 9 | 7 | 5 |
| **Minimal-request friendliness**   | 10 | 7 | 8 |
| **Collapsibility** (graceful degradation when fields omitted) | 9 | 8 | 7 |
| **Extensibility** (adding `opencode:`, new runtimes) | 9 | 9 | 8 |
| **Separation of concerns**         | 8 | 9 | 7 |
| **Three-path parity** (JSON = Go = YAML) | 10 | 9 | 6 |
| **Schema/IDE support** (autocomplete, validation) | 9 | 8 | 5 |
| **Prior art alignment** (Nomad, Docker Compose, k8s) | 9 | 7 | 6 |
| **Total**                          | **73** | **64** | **52** |

### Scoring Rationale

**Alt A wins on minimal-request** because three flat fields (`agent`, `prompt`, `work_dir`)
is the lowest possible friction. Alt B forces `task: { prompt: ... }` nesting.

**Alt A wins on three-path parity** because standard JSON/YAML unmarshalling handles it
without custom logic. Alt C requires custom deserializers.

**Alt B wins on separation of concerns** because the task/infrastructure split is clean.
But it's a marginal win that costs more in daily ergonomics.

**Alt C loses across the board** because it adds complexity (custom unmarshalling, redundant
type field, worse schema support) without solving a real problem.

---

## 4. Final Recommendation

**Alternative A: Refined Flat.** The current shape is fundamentally correct. The refinements are:

| Change | Reason |
|--------|--------|
| Move `pty` to top level | Session behavior, not a resource limit |
| Rename `resources` → `container` | Honest naming — `image` isn't a "resource" |
| Add `name` (human label) | Observability, YAML file ergonomics |
| Add `model` at top level | Cross-agent concept, convenience |
| Change `timeout_ms` → `timeout` (duration string) | Humans write YAML, humans don't think in ms |
| Add `resume_session` | Session resume is a core use case |

### Tradeoffs (Honest)

1. **Flat top-level gets crowded as features grow.** Currently 14 top-level fields is fine.
   At 25+ fields, you'd want grouping. But YAGNI — group when it hurts, not before.

2. **Agent sibling keys mean the Go struct grows with each agent.** Adding `opencode:` means
   adding `OpenCode *OpenCodeConfig` to `SessionRequest`. This is acceptable — it's one line
   per agent, and the registry pattern means the runtime only reads the matching block.

3. **`timeout` as a duration string requires parsing.** Go's `time.ParseDuration` handles this
   natively (`"5m"`, `"300s"`, `"1h30m"`). For JSON consumers, also accept integer milliseconds
   as a fallback.

4. **`container:` is Docker-specific naming.** When SSH or OpenSandbox runtimes arrive, they
   won't have `image:` or `security_opt:`. Options: rename to `runtime_config:` (generic but
   vague) or keep `container:` and add `ssh:` / `sandbox:` sibling blocks later. I'd keep
   `container:` — it's honest about what it is, and other runtimes will have different enough
   config shapes that a shared block would be forced.

### On File Format

**YAML is the right choice.** The use case demands:
- Human authoring (rules out pure JSON)
- Multi-line strings for `claude_md`, `instructions` (YAML's `|` syntax is perfect)
- Comments (YAML has them, JSON doesn't, TOML does)
- Nested structures without ceremony (TOML is painful for deeply nested maps like `settings_json`)
- Ecosystem familiarity (Docker Compose, GitHub Actions, Kubernetes all chose YAML)

TOML would be better for flat configs but worse for nested `settings_json` and `mcp_servers`.
CUE and Jsonnet are powerful but add toolchain dependencies — too heavy for a library that
needs to be consumable by `curl` and `cat task.yaml`.

### On MCP Server Placement

**Top-level is correct.** MCP servers are infrastructure declarations. agentruntime materializes
them into agent-specific formats:
- Claude: merged into `.mcp.json`
- Codex: may become env vars or flags
- OpenCode: TBD

If they lived inside `claude:`, switching `agent: codex` would require moving the MCP block.
Top-level means "these services exist" and the agent adapter figures out how to wire them.

---

## Appendix: Prior Art Comparison

| System | Agent/Driver Config Pattern | Result |
|--------|---------------------------|--------|
| **Nomad** | `config {}` implicitly typed by `driver` field — sibling pattern | Clean, well-proven |
| **Docker Compose** | Flat service-level with some nesting (`deploy.resources`) | Simple but limited extensibility |
| **GitHub Actions** | Flat `runs-on` + nested `steps[]` + `env` at multiple levels | Good for simple, messy at scale |
| **Kubernetes** | Deep nesting (`spec.containers[].resources.limits`) | Powerful but verbose |
| **Terraform** | Provider-namespaced blocks (`aws {}`, `gcp {}`) | Exact same pattern as agent sibling keys |

The Nomad and Terraform patterns are the closest prior art. Both use implicitly-typed sibling
blocks, both are well-regarded, both have survived years of production use at scale. The
agentruntime design follows this pattern correctly.
