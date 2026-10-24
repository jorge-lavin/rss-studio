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
	databasePath := envOrDefault("RSS_STUDIO_DB", filepath.Join(filepath.Dir(dataPath), "rss-studio.db"))
	database, err := storage.NewSQLiteStore(databasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()
	username := os.Getenv("RSS_STUDIO_AUTH_USERNAME")
	password := os.Getenv("RSS_STUDIO_AUTH_PASSWORD")
	if err := database.BootstrapUser(username, password); err != nil {
		log.Fatalf("bootstrap authentication: %v", err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		Username:  username,
		Password:  password,
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
