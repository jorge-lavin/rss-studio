package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	address := envOrDefault("RSS_STUDIO_ADDRESS", ":8080")
	dataPath := envOrDefault("RSS_STUDIO_DATA", "/data/feeds.json")

	store, err := newStore(dataPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	app := newApp(store, &http.Client{
		Timeout:   20 * time.Second,
		Transport: transport,
	})
	server := &http.Server{
		Addr:              address,
		Handler:           app.routes(),
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
