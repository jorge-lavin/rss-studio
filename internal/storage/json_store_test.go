package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJSONStorePersistsFeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feeds.json")
	first, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Create("alice", "News Feed", "https://example.test/rss", "go"); err != nil {
		t.Fatal(err)
	}

	second, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	feed, found := second.Get("news-feed")
	if !found || feed.Owner != "alice" || feed.Keyword != "go" {
		t.Fatalf("persisted feed not found: %#v", feed)
	}
	if feeds := second.List("bob"); len(feeds) != 0 {
		t.Fatalf("user saw another user's feeds: %#v", feeds)
	}
	if err := second.Delete("news-feed", "bob"); err == nil {
		t.Fatal("user deleted another user's feed")
	}
}

func TestJSONStoreKeepsLegacyFeedsPublicButUnowned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feeds.json")
	if err := os.WriteFile(path, []byte(`[{"name":"Legacy feed","slug":"legacy-feed","source_url":"https://example.test/rss","keyword":"go"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := store.Get("legacy-feed"); !found {
		t.Fatal("legacy public feed was lost")
	}
	if feeds := store.List("alice"); len(feeds) != 0 {
		t.Fatalf("legacy feed exposed in account management: %#v", feeds)
	}
}
