# RSS Studio

Self-hosted podcast feed filtering for AntennaPod. RSS Studio keeps episodes whose titles contain a keyword and publishes a stable RSS URL while preserving original GUIDs, enclosures, and podcast metadata.

## Deploy

1. Create a reusable, tagged Tailscale auth key.
2. Copy `.env.example` to `.env` and set `TS_AUTHKEY`.
3. Run `docker compose up -d --build`.
4. Open `https://rss-studio.<your-tailnet>.ts.net` from a device connected to the tailnet.

Feed definitions persist in the `rss-studio-data` Docker volume. Tailscale identity persists in `tailscale-state`.
Tailscale Serve terminates HTTPS and proxies to RSS Studio. Direct tailnet access on port `8080` remains available as a fallback.

Generated feeds are precomputed on disk. Requests return cached XML immediately, then check the source in the background. RSS Studio regenerates only when the source `Last-Modified` value advances.

## Development

Requires Go 1.23 or newer.

```sh
go test ./...
go run .
```

Override storage or listen address with `RSS_STUDIO_DATA` and `RSS_STUDIO_ADDRESS`.
