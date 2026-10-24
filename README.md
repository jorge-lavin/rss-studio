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

Open <http://localhost:8080> to sign in through the web UI using the configured
account. RSS Studio does not support self-registration. For API clients,
authenticate first, then send the returned bearer token for management requests:

```sh
token=$(curl -s http://localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"change-this-password"}' | jq -r .access_token)
curl -H "Authorization: Bearer $token" http://localhost:8080/
```

Generated RSS URLs remain public so RSS readers can fetch them without JWT
support. Feed definitions and generated feeds persist in
the `rss-studio-data` Docker volume. The same volume contains the SQLite
database at `/data/rss-studio.db`; its migrations run automatically at startup.
On first startup, the configured authentication account is stored as a password
hash. Existing accounts are not overwritten by later environment changes.

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

Successful releases deploy the matching SemVer image to the existing VPS over
SSH. Configure these Forgejo repository secrets:

- `GHCR_TOKEN`: package write token used by CI to publish the image; not used by deployment.
- `DEPLOY_SSH_KEY`: private key authorized for the deployment user.
- `DEPLOY_KNOWN_HOSTS`: pinned `known_hosts` entry for the deployment host.
- `DEPLOY_HOST`: VPS hostname or address.
- `DEPLOY_USER`: VPS SSH user.

SSH uses default port 22; no `DEPLOY_PORT` secret is needed. The remote host
must already be logged into GHCR with a read-only package token. Its
`/home/deploy/rss-studio/compose.yaml` must require the deployment tag instead
of providing a `latest` fallback:

```yaml
services:
  rss-studio:
    image: ghcr.io/jorge-lavin/rss-studio:${RSS_STUDIO_TAG:?RSS_STUDIO_TAG is required}
```

The workflow does not log in or modify remote Docker credentials. It pulls and
recreates only the `rss-studio` service, preserving its Docker data volume and
leaving separate Caddy infrastructure untouched.

## Production rollback

Normal release deployment automatically restores the previously running
immutable release image if pulling, starting, or startup verification of the
new image fails. The deployment remains failed so it is visible in Forgejo.
Automatic rollback requires an existing release-tagged `rss-studio` container;
if none exists, deployment stops before changing production.

For manual recovery, use an existing immutable release image. Before starting,
confirm the target release exists in GHCR, the deployment secrets are
configured, and no migration or other release-specific action is required.
Do not change `RSS_STUDIO_AUTH_USERNAME`, `RSS_STUDIO_AUTH_PASSWORD`, or
`RSS_STUDIO_JWT_SECRET`; they remain on the host and existing data stays in the
`rss-studio-data` volume.

In Forgejo, run **Roll back production** from `main`, enter an exact version such as
`0.1.0` or `0.2.0` without the `v` prefix, and start the workflow. It rejects
`latest`, `v0.2.0`, prerelease versions, and other input before opening SSH.
The workflow runs `docker compose pull` followed by `up -d --no-build` for only
the `rss-studio` service.

Verify the result on the host:

```sh
cd /home/deploy/rss-studio
docker compose ps rss-studio
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/
```

With authentication enabled, the curl check should return `401`; also verify
login and one known public `/feeds/*.xml` URL through the normal public host.
If rollback is not sufficient, forward-redeploy by running the same workflow
with the desired exact release version, or merge a fix to `main` and let the
normal release deployment publish and deploy it.

## Development

Requires Go 1.23 or newer.

```sh
go test ./...
go run ./cmd/rss-studio
```

Override storage or listen address with `RSS_STUDIO_DATA` and `RSS_STUDIO_ADDRESS`.
Override the SQLite path with `RSS_STUDIO_DB`. `RSS_STUDIO_AUTH_USERNAME`,
`RSS_STUDIO_AUTH_PASSWORD`, and `RSS_STUDIO_JWT_SECRET` are required. JWTs use
HS256 and expire after 24 hours.
Login attempts are limited to five per client IP per minute; excess attempts
return HTTP 429 with a `Retry-After` header.
