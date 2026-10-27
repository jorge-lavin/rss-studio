# Documentation handoff status

This file records completed documentation handoffs. The tracked README,
OpenAPI specification, and MCP adapter are authoritative for current behavior.

| Handoff | Status | Authoritative documentation |
| --- | --- | --- |
| API-first and agentic usage | Complete | [`README.md`](../README.md), [`openapi.yaml`](openapi.yaml), and the tracked `internal/mcp` adapter |
| Public feed rate limit | Complete | [`README.md`](../README.md), [`openapi.yaml`](openapi.yaml), and the tracked public-feed policy |

The historical handoff originals remain protected, untracked files in the
coordinator worktree. They are intentionally not copied, edited, staged, or
deleted here. No missing `add-mcp-support` work is being recreated; the tracked
MCP adapter is the current implementation.
