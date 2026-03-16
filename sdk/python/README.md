# agentruntime Python SDK

This SDK will be auto-generated from the agentruntime OpenAPI specification
once the API stabilizes. It will provide:

- `AgentRuntimeClient` — HTTP client for session CRUD
- `SessionStream` — WebSocket client for stdio streaming
- Async support via `asyncio`

For now, use `httpx` + `websockets` directly against the agentd API.
