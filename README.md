# RSS Studio

RSS Studio filters podcast feeds by episode title using contains, starts with,
ends with, or equals matching, and publishes stable RSS
feeds while preserving original GUIDs, enclosures, and podcast metadata.

This repository contains the application source, tests, Docker image, and CI.
Host-specific deployment configuration is maintained separately in the
`docker-configs` repository.

## Docker example

The included Compose file builds and runs the application locally. Its host port
binds to loopback only:

```sh
docker compose up -d --build
```

Open <http://localhost:8080> to create an account and log in through the web
UI. Accounts and bcrypt password hashes persist in the SQLite database. For API
clients, register and then authenticate against the versioned API before sending
the returned bearer token for management requests:

```sh
curl -i http://localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"change-this-password"}'
token=$(curl -s http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"change-this-password"}' | jq -r .access_token)
curl -H "Authorization: Bearer $token" http://localhost:8080/api/v1/feeds
```

Generated RSS URLs remain public so RSS readers can fetch them without
credentials. Feed definitions persist in SQLite and are isolated per account, while
generated feeds persist in tenant-scoped cache folders in the `rss-studio-data`
Docker volume. The same volume contains the SQLite database at
`/data/rss-studio.db`; its migrations run automatically at startup.
Accounts created through the web UI or API are stored as password hashes and
remain available after restarts.
If an older deployment has `/data/feeds.json` and an existing account, startup
imports its feeds into the first account and removes the legacy file after a
successful import.

The service-token, idempotency, rate-limit, and quota migrations are additive and run
in one transaction each at startup. There is no destructive down migration: older
binaries reject databases at the newer schema version, so rolling back after
this release requires restoring a pre-migration database backup or deploying a
compatible forward migration first. The quota migration adds account usage
buckets only; it does not remove feeds or reset existing authentication data.
Do not remove the idempotency or quota tables from a live database; that would
discard retry protection or quota state and is not a supported rollback.

Before upgrading, stop the service or take a consistent backup of the
`rss-studio-data` volume, including `/data/rss-studio.db`. Start the new image
with that same volume; migrations run automatically, and existing accounts,
feed definitions, generated-feed cache files, and public feed paths remain in
place. Verify that users can sign in again and that one known public feed still
loads before removing the backup.

Existing browser JWT sessions are not compatible with this release; users must
sign in again after upgrading. Browser
and API credential login intentionally issue a full-scope service token named
`login` so existing browser and management routes continue to work. Use a
separately created token with only required scopes for integrations. Revoking a
token invalidates it immediately; browser logout clears its cookie but does not
revoke the server-side token, so revoke it when immediate invalidation is needed.

Expired and revoked service-token rows are retained for 30 days after their
terminal time for metadata/audit visibility. Startup and token issuance delete
older terminal rows; valid tokens are never removed by this cleanup. This is
automatic and requires no operator job.

To run the published image instead:

```sh
docker run -d \
  --name rss-studio \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v rss-studio-data:/data \
  ghcr.io/jorge-lavin/rss-studio:latest
```

These Compose and published-image examples intentionally expose HTTP on
`127.0.0.1` only. This is a breaking default for operators who relied on port
8080 being reachable from other hosts; local startup and `http://localhost:8080`
examples are unchanged.

### Non-local deployments

Any deployment reachable beyond the local machine must terminate HTTPS at a
trusted reverse proxy or load balancer. Forward only the proxy's internal HTTP
hop to the RSS Studio container; do not expose the container's HTTP port
directly to remote clients. For each HTTPS request, the proxy must set and
preserve `X-Forwarded-Proto: https` and must not accept that header from
untrusted clients. RSS Studio uses it to mark authentication cookies `Secure`
and to generate HTTPS base URLs.

Loopback binding is not a substitute for firewalling or TLS. If you change the
binding to a non-loopback address, restrict it with host/network firewall rules
and still require HTTPS termination at the trusted proxy or load balancer.

Generated feeds are precomputed on disk. Requests return cached XML immediately, then check the source in the background. RSS Studio regenerates only when the source `Last-Modified` value advances.

## Public contract and versioned API

The supported public management contract is [`docs/openapi.yaml`](docs/openapi.yaml).
Completed documentation handoffs are recorded in [`docs/handoff-status.md`](docs/handoff-status.md).
All versioned API requests and responses use JSON. Unknown request fields are
rejected. API errors never redirect to the browser UI.

### Endpoints and credentials

| Method and path | Result | Credential / scope |
| --- | --- | --- |
| `POST /api/v1/auth/login` | Issue service token | Public credential exchange |
| `POST /api/v1/auth/register` | Create account | Public |
| `GET /api/v1/auth/tokens` | List token metadata | Service token with `tokens:manage` |
| `POST /api/v1/auth/tokens` | Create service token | Service token with `tokens:manage` |
| `POST /api/v1/auth/tokens/{id}/revoke` | Revoke service token | Service token with `tokens:manage` |
| `DELETE /api/v1/auth/tokens/{id}` | Revoke service token alias | Service token with `tokens:manage` |
| `GET /api/v1/feeds` | List tenant feeds | Service token with `feeds:read` |
| `POST /api/v1/feeds` | Publish one feed | Service token with `feeds:write` |
| `POST /api/v1/feeds/preview` | Preview outputs | Service token with `feeds:preview` |
| `POST /api/v1/feeds/split` | Publish up to 32 outputs | Service token with `feeds:write` |
| `DELETE /api/v1/feeds/{path}` | Delete tenant feed | Service token with `feeds:delete` |

Send credentials as `Authorization: Bearer <service-token>`. Service tokens are
expiring, revocable bearer credentials. Every management operation requires the
scope granted to its service token; `tokens:manage` controls token management.
Tenant isolation applies to every feed and token operation.

### Accounts and service tokens

Register and log in with JSON:

```sh
BASE_URL=http://localhost:8080
USERNAME=alice-$(date +%s)
PASSWORD='change-this-password'
curl -fsS -X POST "$BASE_URL/api/v1/auth/register" \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg u "$USERNAME" --arg p "$PASSWORD" '{username:$u,password:$p}')" | jq
token=$(curl -fsS -X POST "$BASE_URL/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg u "$USERNAME" --arg p "$PASSWORD" '{username:$u,password:$p}')" | jq -r .access_token)
```

Token creation requires `name`, a non-empty list of valid scopes, and exactly
one of positive `expires_in` seconds or RFC3339 `expires_at`:

```bash
token_json=$(curl -fsS -X POST "$BASE_URL/api/v1/auth/tokens" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  -d '{"name":"feed-agent","scopes":["feeds:read","feeds:preview","feeds:write","feeds:delete"],"expires_in":86400}')
service_token=$(jq -r .token <<<"$token_json")
token_id=$(jq -r .id <<<"$token_json")
jq '{id,name,scopes,created_at,expires_at}' <<<"$token_json"
curl -fsS "$BASE_URL/api/v1/auth/tokens" -H "Authorization: Bearer $token" | jq
```

Supported scopes are `feeds:read`, `feeds:preview`, `feeds:write`,
`feeds:delete`, and `tokens:manage`. Duplicate scopes are removed and returned
in canonical order.
The raw `token` value is returned only by creation; save it in a secret store,
never put it in source control, URLs, logs, or support tickets. Listing tokens
returns `id`, `name`, `scopes`, `created_at`, `expires_at`, and optional
`revoked_at`, never the raw token. `DELETE /api/v1/auth/tokens/{id}` is an
alias for the revoke POST.

### Feed request and response shapes

Create and preview output objects use `name`, `source_url`, `operator`, and
`keyword`. Operators are `contains`, `starts_with`, `ends_with`, and `equals`;
matching is case-insensitive. Omitted operators default to `contains`. Names
are limited to 120 characters, source URLs to 2048 characters, keywords to 200
characters, and source URLs must be HTTP or HTTPS without user information.

`GET /api/v1/feeds` returns `{ "feeds": [...] }`. Each feed has `name`, `path`,
`url`, `source_url`, `operator`, `keyword`, `status`, and RFC3339 `created_at`.
`status` is `ready` or `refreshing` and reports background refresh state for the
feed's source. Create
returns one feed object with HTTP 201. Preview returns
`{ "outputs": [{"name","operator","keyword","count","sample_titles"}] }`;
sample titles contain at most five entries. Split accepts one to 32 outputs and
returns `{ "outputs": [{"name","operator","keyword","path","url","status"}] }`
with HTTP 201. Delete returns `{ "deleted": true, "path": "opaque-id" }`;
new feed paths use 64-character cryptographic hashes and exclude the feed name.

### Stable errors and response metadata

Every versioned API error is:

```json
{"error":{"code":"validation_error","message":"..."},"request_id":"..."}
```

`request_id` is also returned as `X-Request-ID`; a client may supply a safe
request ID in that header. Codes currently returned are:

| HTTP | Codes and meaning |
| --- | --- |
| 400 | `invalid_json`, `unknown_field`, `unsupported_media_type`, `validation_error` |
| 401 | `authentication_required`, `invalid_credentials` |
| 403 | `insufficient_scope`, `forbidden` |
| 404 | `not_found` |
| 405 | `method_not_allowed` |
| 409 | `username_taken`, `idempotency_conflict` |
| 429 | `rate_limited`, `quota_exceeded` |
| 501 | `registration_unavailable`, `service_tokens_unavailable` |
| 502 | `source_unavailable` (upstream could not be fetched or processed) |
| 503 | `rate_limit_unavailable`, `quota_unavailable` |
| 500 | `internal_error` |

Policy failures include `WWW-Authenticate: Bearer`; insufficient-scope
responses identify `insufficient_scope` without disclosing the credential.
Rate-limited API operations include `RateLimit-Limit`,
`RateLimit-Remaining`, `RateLimit-Reset`, and `Retry-After` on the 429 response.
The `X-RateLimit-*` spellings are also emitted for older clients. Reset values
are approximate seconds. Login/registration protection emits `Retry-After` but
not operation rate-limit headers.

Quota errors include safe metadata in `error`: `quota`, `limit`, `remaining`,
`reset_at` (UTC midnight for daily quotas), and `reset_after` seconds. A
`Retry-After` header is returned when a quota has a future reset; the permanent
published-feed ceiling has `reset_after: 0` and no retry delay.

### Idempotency

`POST /api/v1/feeds` and `POST /api/v1/feeds/split` accept optional
`Idempotency-Key` headers, up to 255 characters. The key is bound to both
authenticated owner and credential, and only hashes are stored. With the same
owner, credential, key, and decoded JSON request, a retry returns the original
JSON result and status. Reusing the key with changed JSON returns HTTP 409 and
`idempotency_conflict`; the key is not included in the response. Completed keys
expire after 24 hours. Failed work releases its pending claim so it can be
retried. If mutation succeeds but result persistence fails, the service retries
completion, keeps the claim pending, and returns `idempotency_unavailable`
(HTTP 503) rather than risking a duplicate mutation. Retry with the same key
after storage recovers; never switch to a new key. Pending claims retain their
prepared result for safe completion and are never reclaimed automatically; a
claim without a prepared result requires operator reconciliation before retry.
Do not log raw keys.

### Local API integration examples

The following commands use a public RSS source because the application blocks
private and loopback upstream destinations. Set `SOURCE_URL` to another public
RSS/Atom URL if needed:

```bash
SOURCE_URL=https://feeds.simplecast.com/54nAGcIl
payload=$(jq -nc --arg source "$SOURCE_URL" '{source_url:$source,outputs:[{name:"AI episodes",operator:"contains",keyword:"AI"}]}' )

# List, preview, create, and split.
curl -fsS "$BASE_URL/api/v1/feeds" -H "Authorization: Bearer $token" | jq
curl -fsS -X POST "$BASE_URL/api/v1/feeds/preview" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d "$payload" | jq
created=$(curl -fsS -X POST "$BASE_URL/api/v1/feeds" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d "$(jq -nc --arg source "$SOURCE_URL" '{name:"AI episodes",source_url:$source,operator:"contains",keyword:"AI"}')")
feed_path=$(jq -r .path <<<"$created")
jq <<<"$created"
split=$(curl -fsS -X POST "$BASE_URL/api/v1/feeds/split" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -H 'Idempotency-Key: split-example-1' -d "$payload")
jq <<<"$split"

# Same key and same payload replays; changed payload is a 409 conflict.
create_payload=$(jq -nc --arg source "$SOURCE_URL" '{name:"Retry example",source_url:$source,keyword:"AI"}')
curl -fsS -X POST "$BASE_URL/api/v1/feeds" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -H 'Idempotency-Key: create-example-1' -d "$create_payload" | jq
curl -fsS -X POST "$BASE_URL/api/v1/feeds" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -H 'Idempotency-Key: create-example-1' -d "$create_payload" | jq
curl -sS -o /tmp/rss-studio-conflict.json -w '%{http_code}\n' -X POST "$BASE_URL/api/v1/feeds" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -H 'Idempotency-Key: create-example-1' -d "$(jq -nc --arg source "$SOURCE_URL" '{name:"Changed retry",source_url:$source,keyword:"AI"}')"
jq </tmp/rss-studio-conflict.json

# Delete the single feed. API path parameter accepts the returned public path.
curl -fsS -X DELETE "$BASE_URL/api/v1/feeds/${feed_path#/feeds/}" -H "Authorization: Bearer $token" | jq
```

### Public RSS

Generated URLs do not require authentication. RSS readers need no API
credential or bearer token. Use the `path` or `url` returned by create/split and
give the URL to any RSS reader:

```bash
public_url=$(jq -r '.outputs[0].url' <<<"$split")
curl -fsS "$public_url"
```

Public delivery allows an aggregate 60 requests per feed URL per one-minute
fixed window, shared by all readers of that URL. It does not consume account
quota or authenticated API-rate-limit buckets. Put a reverse proxy in front of
the service before exposing `/feeds/*.xml` to the Internet. Application limiting
does not replace proxy bandwidth controls, IP/request controls, or distributed
denial-of-service protection. The proxy must also preserve the client IP used
for login protection according to its own trusted-proxy policy.

## Limits, quotas, and operations

### Defaults

| Policy | Default |
| --- | --- |
| Login and registration protection | 5 attempts per client IP per one-minute fixed window |
| API cheap reads (`feeds.list`, `auth.tokens.list`) | 120 requests per account and credential per minute |
| API preview | 20 requests per account and credential per minute |
| API delete/revoke | 10 requests per account and credential per minute |
| API create/token creation | 10 requests per account and credential per minute |
| API split | 5 requests per account and credential per minute |
| Public RSS delivery | 60 aggregate requests per feed URL per one-minute fixed window, shared by all readers; no authentication required |
| Published feeds | 50 per account, permanent ceiling |
| Previews | 100 per account per UTC day |
| Single-feed creations | 20 per account per UTC day |
| Split operations | 10 per account per UTC day |
| Concurrent preview/create/split operations | 2 per account |
| Split outputs | 32 |
| JSON request body | 1 MiB; auth JSON body is 16 KiB |
| Source feed response | 25 MiB |
| Generated split aggregate | 128 MiB |

API counters are fixed one-minute windows. They consume one account bucket and
one credential bucket atomically, so several service tokens still share the
account ceiling. Policy classes are `cheap_read`, `delete`, `preview`, and
`create`; split uses the `create` class with its own lower limit of five.
MCP feed tools use the same operation limits; MCP rate-limit failures are safe
tool errors rather than HTTP headers. Login protection is an in-memory
per-process client-IP counter and applies to login and registration.

Quotas are durable SQLite account counters. A create reserves one published
feed, one creation, and one concurrent operation. A split reserves one
published-feed slot per output, one split, and one concurrent operation. A
preview reserves one preview and one concurrent operation. Failed upstream work
refunds reservations; successful deletion decrements the published-feed
counter. Daily counters reset at UTC midnight.

The SQLite rate-limit counters, including public RSS limits, and in-memory
login limiter are designed for the existing single-instance deployment model.
They do not provide a safe distributed coordinator for multiple replicas. Run
one instance or add a shared rate-limit service before horizontal scaling;
also enforce proxy bandwidth, IP/request, and distributed-denial-of-service
controls for public RSS.

### Operator settings

The application reads these environment variables. Empty quota variables keep
their defaults. Compose passes every setting through so local overrides work:

| Variable | Default / behavior |
| --- | --- |
| `RSS_STUDIO_ADDRESS` | `:8080` |
| `RSS_STUDIO_DATA` | `/data`; cache directory and legacy feed location |
| `RSS_STUDIO_DB` | `$RSS_STUDIO_DATA/rss-studio.db` |
| `RSS_STUDIO_MCP_ENABLED` | `true`; set `false` to make `/mcp` return 404 |
| `RSS_STUDIO_MAX_FEEDS_PER_ACCOUNT` | `50` |
| `RSS_STUDIO_PREVIEWS_PER_DAY` | `100` |
| `RSS_STUDIO_CREATIONS_PER_DAY` | `20` |
| `RSS_STUDIO_SPLITS_PER_DAY` | `10` |
| `RSS_STUDIO_MAX_CONCURRENT_EXPENSIVE_OPERATIONS` | `2` |
| `OTEL_SERVICE_NAME` | `rss-studio` when OTLP is enabled |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Empty; standard OTLP/HTTP endpoint enables tracing |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Empty; trace-specific OTLP/HTTP endpoint alternative |
| `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Empty; trace exporter headers such as Axiom authorization and dataset |
| `OTEL_TRACES_EXPORTER` | Set `none` to disable exporting explicitly |

Example local overrides must be exported before the existing Compose command:

```sh
export RSS_STUDIO_MCP_ENABLED=false
export RSS_STUDIO_PREVIEWS_PER_DAY=200
docker compose up -d --build
```

SQLite is created with restrictive file permissions in `/data`; keep the Docker
volume private and back it up before migrations or rollback. `RSS_STUDIO_DB`
must point to a path available inside the container.

## MCP

RSS Studio exposes a stateless Streamable HTTP JSON-RPC MCP endpoint at
`POST /mcp`. It requires an expiring service token in
`Authorization: Bearer <service-token>`; non-service credentials and cookies are
rejected. MCP is enabled by default. Set `RSS_STUDIO_MCP_ENABLED=false` before
`docker compose up -d --build` to make `/mcp` unavailable.

Supported `MCP-Protocol-Version` values are `2024-11-05`, `2025-03-26`,
`2025-06-18`, and `2025-11-25`. If the header is omitted, initialization
selects `2025-03-26`; an unsupported header returns HTTP 400 and JSON-RPC
`-32600`.

The endpoint supports `initialize`, `notifications/initialized`, `ping`,
`tools/list`, and `tools/call`. The five tools and exact inputs are:

| Tool | Scope | Input |
| --- | --- | --- |
| `list_feeds` | `feeds:read` | `{}` |
| `preview_feed` | `feeds:preview` | `{source_url, outputs}`; each output requires `name`, `keyword`, and optional `operator`; 1–32 outputs |
| `create_feed` | `feeds:write` | Required `name`, `source_url`, `keyword`; optional `operator`, `idempotency_key` |
| `create_split_feeds` | `feeds:write` | Required `source_url`, `outputs`; optional `idempotency_key`; 1–32 outputs |
| `delete_feed` | `feeds:delete` | Required `path`; optional `idempotency_key` |

All tool objects reject unknown or JSON-null fields. `operator` accepts the
same four values as the API and defaults to `contains`. `idempotency_key` is a
non-empty value up to 255 characters when supplied. Mutation retries with the
same service token, key, and arguments replay the original result; changed
arguments return an `isError` tool result without exposing the key.

Tool results are JSON text in `result.content[0].text`. `list_feeds` returns an
array of feed objects; `preview_feed` returns an array of preview objects;
`create_feed` returns one feed object; `create_split_feeds` returns an array of
feed objects; `delete_feed` returns `{deleted:true,path}`.

Safe protocol errors use JSON-RPC codes `-32700` (parse), `-32600` (invalid
request), `-32601` (method not found), `-32602` (invalid params), `-32001`
(authentication required), and `-32003` (insufficient scope). Service,
quota, upstream, and rate-limit failures are safe `isError: true` tool results;
they do not include bearer tokens, source content, or private URLs.

Concise local JSON-RPC example using the `service_token` created above:

```sh
curl -fsS -X POST "$BASE_URL/mcp" \
  -H "Authorization: Bearer $service_token" \
  -H 'MCP-Protocol-Version: 2025-03-26' \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_feeds","arguments":{}}}' | jq

# Revoke service token when integration is finished.
curl -fsS -X POST "$BASE_URL/api/v1/auth/tokens/$token_id/revoke" -H "Authorization: Bearer $token" | jq
```

## Observability

RSS Studio records OpenTelemetry-compatible HTTP server spans. Each span includes
the matched route, status, and duration; feed slugs and request paths are not
recorded. Panics become HTTP 500 responses, are logged with a stack trace, and
are recorded as span errors. Requests taking at least one second are marked with
a `slow request` log entry.

Audit events use operation names and numeric account/token IDs rather than
feed slugs. Bearer credentials, passwords, raw idempotency keys, source URLs,
and source feed content are not included in request or audit logs. Treat
request IDs and exported traces as operational data and restrict their access.

Tracing is disabled unless an OTLP endpoint is configured. To export traces over
OTLP/HTTP, set the standard variables before starting the container:

```sh
export OTEL_SERVICE_NAME=rss-studio
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
docker compose up -d --build
```

`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` can be used instead for a trace-specific
endpoint. Set `OTEL_TRACES_EXPORTER=none` to disable exporting explicitly.

For direct Axiom export, use a dedicated traces dataset and keep the token in
your shell or secret manager, never in this repository:

```sh
export AXIOM_TOKEN='your-token'
export AXIOM_DATASET='rss-studio-traces'
export OTEL_SERVICE_NAME=rss-studio
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://api.axiom.co/v1/traces
export OTEL_EXPORTER_OTLP_TRACES_HEADERS="Authorization=Bearer ${AXIOM_TOKEN},X-Axiom-Dataset=${AXIOM_DATASET}"
docker compose up -d --build
```

Unset `AXIOM_TOKEN` and `OTEL_EXPORTER_OTLP_TRACES_HEADERS` after stopping the
service when they are no longer needed.

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

The production host must keep RSS Studio behind that trusted proxy: terminate
HTTPS before forwarding the internal HTTP hop to the container, set
`X-Forwarded-Proto: https`, and do not publish the application port directly to
remote clients. The loopback default does not replace host/network firewall
rules or TLS requirements if the deployment binding is changed.

## Production rollback

Normal release deployment automatically restores the previously running
immutable release image if pulling, starting, or startup verification of the
new image fails. The deployment remains failed so it is visible in Forgejo.
Automatic rollback requires an existing release-tagged `rss-studio` container;
if none exists, deployment stops before changing production.

For manual recovery, use an existing immutable release image. Before starting,
confirm the target release exists in GHCR, the deployment secrets are
configured, and no migration or other release-specific action is required.
Existing service tokens remain in the `rss-studio-data` volume. A release older
than the scoped-token migration cannot open its migrated database; restore the
pre-migration database backup before rolling back across that boundary. Do not
delete the service-token table from a live database.

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
Override the SQLite path with `RSS_STUDIO_DB`. Credential login issues service
tokens that expire after 24 hours by default.
Login attempts are limited to five per client IP per minute; excess attempts
return HTTP 429 with a `Retry-After` header.
Account quotas default to 50 published feeds, 100 previews per UTC day, 20
single-feed creations per UTC day, 10 split operations per UTC day, and two
concurrent expensive operations. Override them with
`RSS_STUDIO_MAX_FEEDS_PER_ACCOUNT`, `RSS_STUDIO_PREVIEWS_PER_DAY`,
`RSS_STUDIO_CREATIONS_PER_DAY`, `RSS_STUDIO_SPLITS_PER_DAY`, and
`RSS_STUDIO_MAX_CONCURRENT_EXPENSIVE_OPERATIONS`.

Authenticated versioned API operations use simple one-minute fixed-window
request limits shared by account and credential. Successful limited responses
include `RateLimit-Limit`, `RateLimit-Remaining`, and `RateLimit-Reset`; a
limited request returns JSON error code `rate_limited`, HTTP 429, and
`Retry-After`. Cheap reads are generous, previews and deletes are lower, and
feed creation/splitting are more restrictive. Public RSS delivery remains
unauthenticated and is limited to an aggregate 60 requests per feed URL per
one-minute fixed window; protect it with reverse-proxy bandwidth, IP/request,
and distributed-denial-of-service controls because application limiting does
not replace them.

Rate-limit counters are stored in the same SQLite database and are intended
for the existing single-instance deployment model. Multiple application
instances do not share a safe distributed rate-limit coordinator; use one
instance or add a shared rate-limit service before scaling horizontally.
