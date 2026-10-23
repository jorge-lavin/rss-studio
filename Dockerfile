FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /rss-studio ./cmd/rss-studio

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /rss-studio /rss-studio
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/rss-studio"]
