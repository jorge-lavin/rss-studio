package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/rss"
	"rss-studio/internal/source"
	"rss-studio/internal/storage"
)

func main() {
	address := envOrDefault("RSS_STUDIO_ADDRESS", ":8080")
	dataPath := envOrDefault("RSS_STUDIO_DATA", "/data/feeds.json")

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
		Handler:           httpapi.NewHandler(service),
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
