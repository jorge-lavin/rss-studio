package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/mcp"
	"rss-studio/internal/observability"
	"rss-studio/internal/rss"
	"rss-studio/internal/source"
	"rss-studio/internal/storage"
)

func main() {
	shutdownObservability, err := observability.Setup(context.Background(), "rss-studio")
	if err != nil {
		log.Fatalf("configure observability: %v", err)
	}
	defer func() {
		if err := shutdownObservability(context.Background()); err != nil {
			log.Printf("shutdown observability: %v", err)
		}
	}()

	address := envOrDefault("RSS_STUDIO_ADDRESS", ":8080")
	configuredData := envOrDefault("RSS_STUDIO_DATA", "/data")
	dataDirectory := configuredData
	legacyFeedPath := filepath.Join(dataDirectory, "feeds.json")
	if strings.HasSuffix(configuredData, ".json") {
		dataDirectory = filepath.Dir(configuredData)
		legacyFeedPath = configuredData
	}
	databasePath := envOrDefault("RSS_STUDIO_DB", filepath.Join(dataDirectory, "rss-studio.db"))
	database, err := storage.NewSQLiteStore(databasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()
	owner, err := database.FirstUser()
	if err == nil {
		if err := database.ImportLegacyFeeds(legacyFeedPath, owner); err != nil {
			log.Fatalf("migrate legacy feeds: %v", err)
		}
	} else if !errors.Is(err, storage.ErrUserNotFound) {
		log.Fatalf("read feed owner: %v", err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		JWTSecret: os.Getenv("RSS_STUDIO_JWT_SECRET"),
		UserStore: database,
	})
	if err != nil {
		log.Fatalf("configure authentication: %v", err)
	}

	service := application.NewServiceWithOperator(
		database,
		cache.NewFileCache(filepath.Join(dataDirectory, "cache")),
		source.NewHTTPSource(&http.Client{Timeout: 20 * time.Second}),
		rss.FilterWithOperator,
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", observability.HTTPHandler(mcp.NewHandler(service, auth)))
	mux.Handle("/", httpapi.NewHandler(service, auth))
	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("RSS Studio listening on %s", address)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
