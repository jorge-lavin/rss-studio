# RSS Studio Contributor Guide

## Scope

RSS Studio is a small Go service that filters podcast feeds by episode title
and publishes cached RSS output for any RSS-compatible reader.

Keep changes small. Prefer the standard library and existing project patterns.

## Files

- `cmd/rss-studio/main.go`: process entrypoint and dependency wiring.
- `internal/domain`: feed model and domain rules.
- `internal/application`: use cases and dependency interfaces.
- `internal/httpapi`: HTTP routes and HTML UI.
- `internal/rss`: RSS parsing and title filtering.
- `internal/source`: upstream HTTP client and outbound URL policy.
- `internal/storage`: JSON feed store.
- `internal/cache`: generated-feed file cache.
- `compose.yaml`: generic local/container deployment.
- `.forgejo/workflows/ci.yml`: tests, image build, and GHCR publishing.

## Verify

Run before pushing:

```sh
go test ./...
docker build -t rss-studio .
```

CI runs both checks. Keep tests deterministic; use `httptest` for upstream RSS
sources.

## Secrets and data

- Never commit `.env`, credentials, tokens, or generated `/data` files.
- Keep credentials in Forgejo Actions secrets or local environment files.
- Do not expose feed definitions or cached content in logs or error messages.

## Images and deployment

- GHCR image: `ghcr.io/jorge-lavin/rss-studio`.
- Main-branch CI publishes both commit-SHA and `latest` tags.
- Use commit-SHA tags for reproducible VPS deployments.
- Keep runtime state in the Docker volume, not in the repository.

Use Conventional Commits (`feat:`, `fix:`, `ci:`, `docs:`, `chore:`).
