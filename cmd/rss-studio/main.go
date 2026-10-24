package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/httpapi"
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
	dataPath := envOrDefault("RSS_STUDIO_DATA", "/data/feeds.json")
	databasePath := envOrDefault("RSS_STUDIO_DB", filepath.Join(filepath.Dir(dataPath), "rss-studio.db"))
	database, err := storage.NewSQLiteStore(databasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		JWTSecret: os.Getenv("RSS_STUDIO_JWT_SECRET"),
		UserStore: database,
	})
	if err != nil {
		log.Fatalf("configure authentication: %v", err)
	}

	store, err := storage.NewJSONStore(dataPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	service := application.NewService(
		store,
		cache.NewFileCache(filepath.Join(filepath.Dir(dataPath), "cache")),
		source.NewHTTPSource(&http.Client{Timeout: 20 * time.Second}),
		rss.Filter,
	)
	server := &http.Server{
		Addr:              address,
		Handler:           httpapi.NewHandler(service, auth),
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
