package storage

import (
	"path/filepath"
	"testing"
)

func TestJSONStorePersistsFeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feeds.json")
	first, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Create("News Feed", "https://example.test/rss", "go"); err != nil {
		t.Fatal(err)
	}

	second, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	feed, found := second.Get("news-feed")
	if !found || feed.Keyword != "go" {
		t.Fatalf("persisted feed not found: %#v", feed)
	}
}
