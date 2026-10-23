# RSS Studio Contributor Guide

## Scope

RSS Studio is a small Go service that filters podcast feeds by episode title
and publishes cached RSS output for any RSS-compatible reader.

Keep changes small. Prefer the standard library and existing project patterns.

## Files

- `main.go`: process entrypoint and configuration.
- `app.go`: HTTP routes and HTML UI.
- `cache.go`: source refresh and generated-feed cache.
- `filter.go`: RSS parsing and title filtering.
- `store.go`: persisted feed definitions.
- `compose.yaml`: local/container deployment.
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

- Never commit `.env`, Tailscale auth keys, tokens, or generated `/data` files.
- Keep credentials in Forgejo Actions secrets or local environment files.
- Do not expose feed definitions or cached content in logs or error messages.

## Images and deployment

- GHCR image: `ghcr.io/jorge-lavin/rss-studio`.
- Main-branch CI publishes both commit-SHA and `latest` tags.
- Use commit-SHA tags for reproducible VPS deployments.
- Keep runtime state in the Docker volume, not in the repository.

Use Conventional Commits (`feat:`, `fix:`, `ci:`, `docs:`, `chore:`).
