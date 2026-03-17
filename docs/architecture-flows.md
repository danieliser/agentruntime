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
    AgentD -->|docker run -d -p 0:9090| DockerRT
    DockerRT -->|WS dial| Container
    Container --- Sidecar

    subgraph Container
        Sidecar -->|spawn + stdio| Agent[Agent Process]
        MCP[IDE MCP WS :random] -.->|tools/context| Agent
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
    H->>S: WS connect ws://container:9090/ws

    Note over S: Start MCP server on random port
    S->>MCP: Listen on :N, write lock file

    Note over S: Spawn Claude with dual channels
    S->>CL: claude --output-format stream-json<br/>--input-format stream-json<br/>--verbose --dangerously-skip-permissions<br/>--ide --session-id {uuid}

    CL->>MCP: WS connect (Channel A: tools)
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
    H->>S: WS connect ws://container:9090/ws

    Note over S: Spawn Codex app-server
    S->>CX: codex app-server --listen stdio://
    S->>CX: {method:"initialize", id:0, params:{clientInfo:{name:"agentruntime"}}}
    CX->>S: {id:0, result:{userAgent:"codex-cli/0.115.0"}}
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
```

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
    subgraph "Host (agentd)"
        HD[~/.local/share/agentruntime/]
        CS[claude-sessions/{session-id}/]
        CXS[codex-sessions/{session-id}/]
        LOGS[logs/{session-id}.ndjson]
        CREDS[credentials/claude-credentials.json]
    end

    subgraph "Container (/home/agent)"
        CC[.claude/ → session dir mount rw]
        CCred[.claude/.credentials.json]
        CSet[.claude/settings.json]
        CMD[.claude/CLAUDE.md]
        CMcp[.claude/.mcp.json]
        CProj[.claude/projects/{hash}/]
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

| Layer | Claude | Codex |
|-------|--------|-------|
| **Spawn** | `claude --output-format stream-json --input-format stream-json --ide` | `codex app-server --listen stdio://` |
| **Output channel** | JSONL on stdout (Channel B) | JSON-RPC notifications on stdout |
| **Input channel** | JSONL on stdin | JSON-RPC requests on stdin |
| **Tool channel** | IDE MCP WebSocket (Channel A) | Same JSON-RPC channel |
| **Steering** | interrupt control_request + new user message | `turn/steer` (native) |
| **Context injection** | `selection_changed` via MCP WS | Not supported natively |
| **Session resume** | `--session-id` + `--resume` | `thread/resume` JSON-RPC |
| **Auth** | OAuth via credentials.json mount | OAuth via auth.json mount |
| **Output format** | Anthropic API message objects | Codex item events with deltas |
