FROM golang:1.27-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X rss-studio/internal/version.Version=${VERSION}" -o /rss-studio ./cmd/rss-studio
RUN install -D -m 0600 -o 65532 -g 65532 /dev/null /data/rss-studio.db \
    && chown 65532:65532 /data \
    && chmod 0700 /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /rss-studio /rss-studio
COPY --from=build --chown=65532:65532 /data /data
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/rss-studio"]
