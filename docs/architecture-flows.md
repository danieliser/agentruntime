# Architecture Flow Diagrams

## System Overview

```mermaid
graph TB
    Client[Client / PAOP / Web UI]
    AgentD[agentd :8090]
    DockerRT[DockerRuntime]
    Container[Docker Container]
    Sidecar[Sidecar v2 :9090]

    Client -->|HTTP/WS| AgentD
    AgentD -->|"docker run -d -p 0:9090<br/>(ephemeral host port)"| DockerRT
    DockerRT -->|"docker port → host:N<br/>WS dial localhost:N"| Sidecar

    subgraph Container
        Sidecar -->|spawn + stdio| Agent[Agent Process]
        MCP["IDE MCP WS :random<br/>(localhost only)"] -.->|tools/context| Agent
    end
```

## Claude Code Flow

```mermaid
sequenceDiagram
    participant C as Client
    participant H as agentd (host)
    participant S as Sidecar
    participant MCP as IDE MCP Server
    participant CL as Claude CLI

    C->>H: POST /sessions {agent:"claude", prompt:"fix the bug"}
    H->>H: docker run -d -p 0:9090 agentruntime-agent
    H->>S: docker port → host:N, WS dial localhost:N/ws

    Note over S: Start MCP server on random port (localhost only)
    S->>MCP: Listen on 127.0.0.1:0, write lock file

    Note over S: Spawn Claude with dual simultaneous channels
    S->>CL: claude --output-format stream-json<br/>--input-format stream-json<br/>--verbose --include-partial-messages<br/>--dangerously-skip-permissions<br/>--ide --session-id {uuid}
    Note right of CL: Channel A: MCP WS on localhost:N<br/>(tools, context injection, selection_changed)<br/>Channel B: stdio JSONL<br/>(all input/output, prompts, results)

    CL->>MCP: WS connect (Channel A: tools + context)
    MCP->>CL: initialize response + tools list

    C->>H: WS: {type:"prompt", data:{content:"fix the bug"}}
    H->>S: forward prompt
    S->>CL: stdin: {type:"user", message:{role:"user",<br/>content:[{type:"text",text:"fix the bug"}]}}

    CL->>S: stdout: {type:"system", subtype:"init"}
    S->>H: {type:"system", data:{subtype:"init"}}
    H->>C: forward

    CL->>S: stdout: {type:"assistant", message:{content:[<br/>{type:"text",text:"I'll fix the auth module"},<br/>{type:"tool_use",name:"Edit",input:{...}}]}}
    S->>H: {type:"agent_message", data:{text:"I'll fix...",<br/>usage:{input_tokens:N, output_tokens:N}}}
    H->>C: forward

    S->>H: {type:"tool_use", data:{name:"Edit", id:"toolu_..."}}
    H->>C: forward

    CL->>MCP: tools/call: getDiagnostics
    MCP->>CL: {content:[{type:"text",text:"0 errors"}]}

    Note over S: Auto-approve tool permission requests
    CL->>S: stdout: {type:"control_request",<br/>request:{subtype:"can_use_tool", request_id:"..."}}
    S->>CL: stdin: {type:"control_response",<br/>response:{request_id:"...", behavior:"allow"}}
    Note right of S: Sidecar auto-approves all can_use_tool<br/>requests — never forwarded to client

    CL->>S: stdout: {type:"result", subtype:"success",<br/>cost_usd:0.02, session_id:"..."}
    S->>H: {type:"result", data:{cost_usd:0.02, session_id:"..."}}
    H->>C: forward

    Note over C: Steering (interrupt + re-prompt)
    C->>H: WS: {type:"interrupt"}
    H->>S: forward
    S->>CL: stdin: {type:"control_request",<br/>request:{subtype:"interrupt"}}

    C->>H: WS: {type:"prompt", data:{content:"focus on DB instead"}}
    H->>S: forward
    S->>CL: stdin: {type:"user", message:{...}}
```

## Codex Flow

```mermaid
sequenceDiagram
    participant C as Client
    participant H as agentd (host)
    participant S as Sidecar
    participant CX as Codex app-server

    C->>H: POST /sessions {agent:"codex", prompt:"fix the bug"}
    H->>H: docker run -d -p 0:9090 agentruntime-agent
    H->>S: docker port → host:N, WS dial localhost:N/ws

    Note over S: Spawn Codex (app-server or exec mode)
    alt Interactive mode (no prompt in request)
    S->>CX: codex app-server --listen stdio:// [--model M]
    S->>CX: {method:"initialize", id:0, params:{clientInfo:<br/>{name:"agentruntime",version:"0.3.0"},<br/>capabilities:{experimentalApi:true}}}
    CX->>S: {id:0, result:{userAgent:"codex-cli/..."}}
    S->>CX: {method:"initialized"}

    C->>H: WS: {type:"prompt", data:{content:"fix the bug"}}
    H->>S: forward prompt

    Note over S: Create thread + start turn
    S->>CX: {method:"thread/start", id:1}
    CX->>S: {id:1, result:{thread:{id:"tid-123"}}}
    CX->>S: notification: {method:"thread/started", params:{id:"tid-123"}}
    S->>H: {type:"system", data:{subtype:"thread_started"}}
    H->>C: forward

    S->>CX: {method:"turn/start", id:2, params:{<br/>threadId:"tid-123",<br/>input:[{type:"text",text:"fix the bug"}],<br/>approvalPolicy:"never",<br/>sandboxPolicy:{type:"dangerFullAccess"}}}

    CX->>S: notification: {method:"item/agentMessage/delta",<br/>params:{delta:"I'll",turnId:"turn-456"}}
    S->>H: {type:"agent_message", data:{delta:"I'll",turnId:"turn-456"}}
    H->>C: forward (streaming)

    CX->>S: notification: {method:"item/started",<br/>params:{item:{type:"commandExecution",command:"git diff"}}}
    S->>H: {type:"tool_use", data:{item:{tool:"commandExecution"}}}
    H->>C: forward

    CX->>S: notification: {method:"item/completed",<br/>params:{item:{type:"commandExecution",exitCode:0}}}
    S->>H: {type:"tool_result", data:{...}}
    H->>C: forward

    CX->>S: notification: {method:"turn/completed",<br/>params:{status:"completed",usage:{input_tokens:N}}}
    S->>H: {type:"result", data:{turn:{status:"completed"}}}
    H->>C: forward

    Note over C: Mid-turn steering (native)
    C->>H: WS: {type:"steer", data:{content:"focus on DB"}}
    H->>S: forward steer
    S->>CX: {method:"turn/steer", id:3, params:{<br/>threadId:"tid-123",<br/>expectedTurnId:"turn-456",<br/>input:[{type:"text",text:"focus on DB"}]}}

    else Prompt mode (fire-and-forget)
    Note over S: codex exec --json --full-auto [--model M]<br/>--skip-git-repo-check "fix the bug"
    S->>CX: spawn process, close stdin
    CX->>S: JSONL: {type:"thread.started",...}
    CX->>S: JSONL: {type:"item.started",...}
    CX->>S: JSONL: {type:"item.completed",...}
    CX->>S: JSONL: {type:"turn.completed",...}
    Note right of S: No JSON-RPC handshake, no steering,<br/>no stdin input. Flat JSONL events only.
    end
```

## Event Normalization

All agent events pass through per-agent normalization (`cmd/sidecar/normalize.go`) before reaching the external WS endpoint. Both Claude and Codex backends emit raw, agent-specific events; the sidecar's `normalizeEvent()` maps them to the standard `NormalizedAgentMessage`, `NormalizedToolUse`, `NormalizedToolResult`, and `NormalizedResult` shapes. Clients always receive the unified schema regardless of which agent produced the event.

## Unified External WS Protocol

```mermaid
graph LR
    subgraph "Client → Sidecar (Commands)"
        P[prompt] -->|content| S[Sidecar]
        I[interrupt] --> S
        ST[steer] -->|content| S
        CTX[context] -->|text, filePath| S
        M[mention] -->|filePath, lines| S
    end

    subgraph "Sidecar → Client (Events)"
        S --> AM[agent_message<br/>text, usage, delta, final]
        S --> TU[tool_use<br/>name, id, input]
        S --> TR[tool_result<br/>tool_use_id, output]
        S --> R[result<br/>cost, duration, session_id]
        S --> PR[progress<br/>status, message]
        S --> SY[system<br/>subtype, session_id]
        S --> ER[error<br/>message, code]
        S --> EX[exit<br/>code]
    end
```

## Container Filesystem Layout

```mermaid
graph TD
    subgraph "Host agentd"
        HD["~/.local/share/agentruntime/"]
        CS["claude-sessions/session-id/"]
        CXS["codex-sessions/session-id/"]
        LOGS["logs/session-id.ndjson"]
        CREDS["credentials/claude-credentials.json"]
    end

    subgraph "Container (/home/agent)"
        CC[.claude/ → session dir mount rw]
        CCred[.claude/.credentials.json]
        CSet[.claude/settings.json]
        CMD[.claude/CLAUDE.md]
        CMcp[.claude/.mcp.json]
        CProj[.claude/projects/hash/]
        CState[.claude.json → account state ro]

        CXC[.codex/ → session dir mount rw]
        CXAuth[.codex/auth.json]
        CXConf[.codex/config.toml]

        WS[/workspace/ → workdir mount rw]
        WSGit[/workspace/.git/ → trust bypass]
    end

    HD --> CS
    HD --> CXS
    HD --> LOGS
    CS -->|mount| CC
    CXS -->|mount| CXC
```

## Data Flow Summary

| Layer | Claude | Codex (app-server) | Codex (exec) |
|-------|--------|-------|------|
| **Spawn** | `claude --output-format stream-json --input-format stream-json --verbose --include-partial-messages --dangerously-skip-permissions --ide --session-id {uuid}` | `codex app-server --listen stdio:// [--model M]` | `codex exec --json --full-auto --skip-git-repo-check [--model M] "prompt"` |
| **Output channel** | JSONL on stdout (Channel B — simultaneous with MCP WS) | JSON-RPC notifications on stdout | Flat JSONL on stdout |
| **Input channel** | JSONL on stdin (Channel B) | JSON-RPC requests on stdin | None (stdin closed) |
| **Tool channel** | IDE MCP WebSocket on localhost:random (Channel A — simultaneous with stdio) | Same JSON-RPC channel | N/A |
| **Steering** | interrupt control_request + new user message | `turn/steer` (native) | Not supported |
| **Tool approval** | `control_request` with `can_use_tool` auto-approved by sidecar | `requestApproval` auto-accepted by sidecar | N/A (`--full-auto`) |
| **Context injection** | `selection_changed` via MCP WS | Not supported natively | Not supported |
| **Session resume** | `--session-id` (always set; `--resume` not yet wired) | `thread/resume` JSON-RPC (not yet wired) | N/A (one-shot) |
| **Auth** | OAuth via credentials.json mount | OAuth via auth.json mount | OAuth via auth.json mount |
| **Output format** | Anthropic API message objects | Codex item events with deltas | Codex flat JSONL events |
