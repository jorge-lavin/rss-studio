FROM golang:1.23-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X rss-studio/internal/version.Version=${VERSION}" -o /rss-studio ./cmd/rss-studio
RUN install -D -m 0600 /dev/null /data/rss-studio.db

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /rss-studio /rss-studio
COPY --from=build /data/rss-studio.db /data/rss-studio.db
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/rss-studio"]
