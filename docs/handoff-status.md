# Documentation handoff status

Status at `origin/main` commit `1f54042`:

| Handoff | Status | Authoritative documentation |
| --- | --- | --- |
| API-first and agentic usage | Complete | [`README.md`](../README.md), [`openapi.yaml`](openapi.yaml), and the tracked `internal/mcp` adapter |
| Public feed rate limit | Complete | [`README.md`](../README.md), [`openapi.yaml`](openapi.yaml), and the tracked public-feed policy |

The historical handoff originals remain protected, untracked files in the
coordinator worktree. They are intentionally not copied, edited, staged, or
deleted here. No missing `add-mcp-support` work is being recreated; the tracked
MCP adapter is the current implementation.

## Consolidation workspace inventory

Inventory at `origin/main` `1f54042` on 2026-10-26. No cleanup was performed.

| Worktree | Branch | State | Proposal |
| --- | --- | --- | --- |
| `/home/jorge/orca/workspaces/rss-studio` | `main` | Baseline worktree; protected untracked handoffs and `json` are present | Retain; do not stage or remove user files |
| `/home/jorge/orca/workspaces/rss-studio/consolidation-finish` | `jorge-lavin/consolidation-finish` | Active focused consolidation PR | Retain until PR merge and owner review |

Forgejo reports PRs `#3` through `#52` merged. Remote refs whose tips are
patch-equivalent to current `origin/main` include the historical task,
consolidation, auth, API, CI, UI, and deployment branches, including
`jorge-lavin/consolidation-dead-code`, `jorge-lavin/consolidation-docs`,
`jorge-lavin/consolidation-release-audit`, `jorge-lavin/hash-feed-url`,
`jorge-lavin/add-api-section`, `jorge-lavin/add-mcp-support`,
the `jorge-lavin/rss-studio-task-*` refs, `pr-51-head`, and
`rate-limit-requests`. These refs are superseded by merged history but remain
undeleted pending owner confirmation.

Refs retaining non-equivalent commits and requiring explicit owner review are
`feature/rss-split`, `fix-image-ci`,
`jorge-lavin/add-filter-operators-starts-with-contains`,
`jorge-lavin/add-github-image`, `jorge-lavin/implement-dark-mode`,
`jorge-lavin/multi-user-feed-storage`,
`jorge-lavin/opentelemetry-observability`,
`jorge-lavin/remove-browser-jwt-service-scoped`, and
`jorge-lavin/rss-studio-task3`. Proposed cleanup: after confirming no linked
worktree, open PR, or owner dependency, remove only patch-equivalent remote
refs; retain unique refs and all protected untracked files until their owners
approve relocation or removal.
