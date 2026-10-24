# RSS Studio

RSS Studio filters podcast feeds by episode title and publishes stable RSS
feeds while preserving original GUIDs, enclosures, and podcast metadata.

This repository contains the application source, tests, Docker image, and CI.
Host-specific deployment configuration is maintained separately in the
`docker-configs` repository.

## Docker example

The included Compose file builds and runs the application locally:

```sh
export RSS_STUDIO_AUTH_USERNAME=admin
export RSS_STUDIO_AUTH_PASSWORD='change-this-password'
export RSS_STUDIO_JWT_SECRET="$(openssl rand -base64 32)"
docker compose up -d --build
```

Authenticate with the API, then send the returned bearer token for management
requests:

```sh
token=$(curl -s http://localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"change-this-password"}' | jq -r .access_token)
curl -H "Authorization: Bearer $token" http://localhost:8080/
```

Generated RSS URLs remain public so RSS readers can fetch them without JWT
support. Feed definitions and generated feeds persist in
the `rss-studio-data` Docker volume.

To run the published image instead:

```sh
docker run -d \
  --name rss-studio \
  --restart unless-stopped \
  -p 8080:8080 \
  -e RSS_STUDIO_AUTH_USERNAME=admin \
  -e RSS_STUDIO_AUTH_PASSWORD='change-this-password' \
  -e RSS_STUDIO_JWT_SECRET='replace-with-at-least-32-random-bytes' \
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
`RSS_STUDIO_AUTH_USERNAME`, `RSS_STUDIO_AUTH_PASSWORD`, and
`RSS_STUDIO_JWT_SECRET` are required. JWTs use HS256 and expire after 24 hours.
