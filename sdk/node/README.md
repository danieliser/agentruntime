# agentruntime Node SDK

This SDK will be auto-generated from the agentruntime OpenAPI specification
once the API stabilizes. It will provide:

- `AgentRuntimeClient` — HTTP client for session CRUD
- `SessionStream` — WebSocket client for stdio streaming
- TypeScript types generated from OpenAPI

For now, use `fetch` + the native `WebSocket` API directly against agentd.
