# RSS Studio

Self-hosted podcast feed filtering. RSS Studio keeps episodes whose titles contain a keyword and publishes a stable RSS URL while preserving original GUIDs, enclosures, and podcast metadata.

## Deploy

1. Run `docker compose up -d --build`.
2. Open `http://localhost:8080` or place a reverse proxy in front of the service.

Feed definitions persist in the `rss-studio-data` Docker volume.

Generated feeds are precomputed on disk. Requests return cached XML immediately, then check the source in the background. RSS Studio regenerates only when the source `Last-Modified` value advances.

## Development

Requires Go 1.23 or newer.

```sh
go test ./...
go run ./cmd/rss-studio
```

Override storage or listen address with `RSS_STUDIO_DATA` and `RSS_STUDIO_ADDRESS`.
