# RSS Studio

RSS Studio filters podcast feeds by episode title and publishes stable RSS
feeds while preserving original GUIDs, enclosures, and podcast metadata.

This repository contains the application source, tests, Docker image, and CI.
Host-specific deployment configuration is maintained separately in the
`docker-configs` repository.

## Docker example

The included Compose file builds and runs the application locally:

```sh
docker compose up -d --build
```

Open <http://localhost:8080>. Feed definitions and generated feeds persist in
the `rss-studio-data` Docker volume.

To run the published image instead:

```sh
docker run -d \
  --name rss-studio \
  --restart unless-stopped \
  -p 8080:8080 \
  -v rss-studio-data:/data \
  ghcr.io/jorge-lavin/rss-studio:latest
```

Place a reverse proxy in front of the container when serving it beyond a local
development environment.

Generated feeds are precomputed on disk. Requests return cached XML immediately, then check the source in the background. RSS Studio regenerates only when the source `Last-Modified` value advances.

## Releases

Forgejo creates releases automatically from `main` after tests pass. Commit
messages follow [Conventional Commits](https://www.conventionalcommits.org/):

- `fix:` or `perf:` creates a patch release.
- `feat:` creates a minor release.
- `BREAKING CHANGE:` or `!` creates a major release.
- Documentation, CI, test, and chore commits do not create releases.

Releases use `vMAJOR.MINOR.PATCH` Git tags and publish matching
`MAJOR.MINOR.PATCH` image tags plus `latest`. Main-branch builds publish only
their commit-SHA image tag.

If no release tag exists, the first successful `main` build creates `v0.1.0`
automatically. No manual bootstrap tag or release token is required; Forgejo's
workflow token creates the release.

## Development

Requires Go 1.23 or newer.

```sh
go test ./...
go run ./cmd/rss-studio
```

Override storage or listen address with `RSS_STUDIO_DATA` and `RSS_STUDIO_ADDRESS`.
