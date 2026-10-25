# RSS Studio

RSS Studio filters podcast feeds by episode title using contains, starts with,
ends with, or equals matching, and publishes stable RSS
feeds while preserving original GUIDs, enclosures, and podcast metadata.

This repository contains the application source, tests, Docker image, and CI.
Host-specific deployment configuration is maintained separately in the
`docker-configs` repository.

## Docker example

The included Compose file builds and runs the application locally:

```sh
export RSS_STUDIO_JWT_SECRET="$(openssl rand -base64 32)"
docker compose up -d --build
```

Open <http://localhost:8080> to create an account and log in through the web
UI. Accounts and bcrypt password hashes persist in the SQLite database. For API
clients, register and then authenticate before sending the returned bearer token
for management requests:

```sh
curl -i http://localhost:8080/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"change-this-password"}'
token=$(curl -s http://localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"change-this-password"}' | jq -r .access_token)
curl -H "Authorization: Bearer $token" http://localhost:8080/
```

Generated RSS URLs remain public so RSS readers can fetch them without JWT
support. Feed definitions persist in SQLite and are isolated per account, while
generated feeds persist in tenant-scoped cache folders in the `rss-studio-data`
Docker volume. The same volume contains the SQLite database at
`/data/rss-studio.db`; its migrations run automatically at startup.
Accounts created through the web UI or API are stored as password hashes and
remain available after restarts.
If an older deployment has `/data/feeds.json` and an existing account, startup
imports its feeds into the first account and removes the legacy file after a
successful import.

The service-token migration is additive and runs in one transaction at startup.
There is no destructive down migration: older binaries reject databases at the
newer schema version, so rollbacks require restoring a pre-migration database
backup or deploying a compatible forward migration first.

To run the published image instead:

```sh
docker run -d \
  --name rss-studio \
  --restart unless-stopped \
  -p 8080:8080 \
  -e RSS_STUDIO_JWT_SECRET='replace-with-at-least-32-random-bytes' \
  -v rss-studio-data:/data \
  ghcr.io/jorge-lavin/rss-studio:latest
```

Place a reverse proxy in front of the container when serving it beyond a local
development environment.

Generated feeds are precomputed on disk. Requests return cached XML immediately, then check the source in the background. RSS Studio regenerates only when the source `Last-Modified` value advances.

## Split feeds

One source feed can produce multiple public RSS outputs. Each output has its own
name, operator, and keyword. Supported operators are `contains`, `starts_with`,
`ends_with`, and `equals`; matching is case-insensitive. Each output gets its
own cached RSS file and public
`/feeds/*.xml` URL. Background refreshes for outputs sharing a source are
coalesced per tenant, so the source is refreshed once for that group. A split
request can contain at most 32 outputs.

### Web UI

After signing in, use **Split feeds** on the home page:

1. Enter the source RSS URL and add output rows with a name, operator, and title keyword.
2. Select **Preview split** to see each output's matching episode count and
   sample titles.
3. Select **Publish split feeds** to create the outputs and receive their public
   URLs.

### JSON API

`POST /preview` and `POST /feeds/split` are authenticated management endpoints;
send the bearer token returned by `/auth/login`. Both accept JSON in this shape:

```json
{
  "source_url": "https://example.com/podcast.xml",
  "outputs": [
    {"name": "AI episodes", "operator": "contains", "keyword": "AI"},
    {"name": "Updates", "operator": "ends_with", "keyword": "update"}
  ]
}
```

Preview the matches without publishing:

```sh
curl -sS -X POST http://localhost:8080/preview \
  -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' \
  -d '{"source_url":"https://example.com/podcast.xml","outputs":[{"name":"AI episodes","operator":"contains","keyword":"AI"}]}'
```

The response contains `outputs` entries with `name`, `operator`, `keyword`,
`count`, and `sample_titles` for each requested output.

Publish the split outputs:

```sh
curl -sS -X POST http://localhost:8080/feeds/split \
  -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' \
  -d '{"source_url":"https://example.com/podcast.xml","outputs":[{"name":"AI episodes","operator":"contains","keyword":"AI"}]}'
```

The response contains `outputs` entries with each output's `name`, `operator`,
`keyword`, public `path`, and absolute `url`.

The existing single-output `POST /feeds` endpoint remains available. Its form
fields are `name`, `source_url`, `operator`, and `keyword`; omitted operators
default to `contains`.

### Service tokens

Authenticated owners can create and manage agent tokens through the versioned
API. Creation requires `name`, `scopes`, and exactly one expiry: `expires_in`
(seconds) or RFC3339 `expires_at`:

```sh
curl -sS -X POST http://localhost:8080/api/v1/auth/tokens \
  -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' \
  -d '{"name":"feed-agent","scopes":["feeds:read","feeds:preview"],"expires_in":86400}'
```

Supported scopes are `feeds:read`, `feeds:preview`, `feeds:write`, and
`feeds:delete`. The raw token is returned only by creation; `GET
/api/v1/auth/tokens` returns metadata and scopes, and `POST
/api/v1/auth/tokens/{id}/revoke` revokes one token. Tokens are bearer
credentials, expire automatically, and are stored only as SHA-256 hashes.

## MCP

RSS Studio exposes a stateless Streamable HTTP MCP endpoint at `/mcp`. It uses
the same bearer token as the JSON API; configure an MCP client with the URL
`https://your-host.example/mcp` and the token returned by `/auth/login`.

Available tools are `list_feeds`, `preview_feed`, `create_feed`,
`create_split_feeds`, and `delete_feed`. Feed management stays scoped to the
authenticated account. The endpoint supports MCP protocol versions
`2024-11-05`, `2025-03-26`, `2025-06-18`, and `2025-11-25`.

## Observability

RSS Studio records OpenTelemetry-compatible HTTP server spans. Each span includes
the matched route, status, and duration; feed slugs and request paths are not
recorded. Panics become HTTP 500 responses, are logged with a stack trace, and
are recorded as span errors. Requests taking at least one second are marked with
a `slow request` log entry.

Tracing is disabled unless an OTLP endpoint is configured. To export traces over
OTLP/HTTP, set the standard variables before starting the container:

```sh
export OTEL_SERVICE_NAME=rss-studio
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
docker compose up -d --build
```

`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` can be used instead for a trace-specific
endpoint. Set `OTEL_TRACES_EXPORTER=none` to disable exporting explicitly.

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

### Why CI builds the image twice

The `image` job runs after tests for pull requests and pushes to `main`. On pull
requests it validates the Dockerfile without publishing; on `main` it publishes
an immutable commit-SHA image. This keeps every main-branch build available for
traceability, even when a commit does not produce a release.

The `release` job runs only after that image succeeds and a release version has
been determined. It builds again with the semantic version embedded in the
binary, then publishes the version and `latest` tags. Building again also keeps
release creation after successful image validation.

The two builds could be consolidated, but only by changing one of these
guarantees: determine the release version before building, or promote the
commit-SHA image after building. Promotion would avoid the rebuild but would
leave the application reporting its development/commit build version instead of
the semantic release version. The duplicate build is therefore intentional.

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
Do not change `RSS_STUDIO_JWT_SECRET`; it remains on the host and existing data
stays in the `rss-studio-data` volume.

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

Override data directory or listen address with `RSS_STUDIO_DATA` and
`RSS_STUDIO_ADDRESS`.
Override the SQLite path with `RSS_STUDIO_DB`. `RSS_STUDIO_JWT_SECRET` is
required. JWTs use HS256 and expire after 24 hours.
Login attempts are limited to five per client IP per minute; excess attempts
return HTTP 429 with a `Retry-After` header.
