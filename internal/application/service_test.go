package application

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
)

type sourceStub struct {
	getCount atomic.Int32
	head     time.Time
}

func (s *sourceStub) Head(context.Context, string) (time.Time, error) {
	return s.head, nil
}

func (s *sourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	s.getCount.Add(1)
	return []byte(`<rss version="2.0"><channel><title>Source</title><item><title>Keep foo</title><guid>1</guid></item><item><title>Drop</title><guid>2</guid></item></channel></rss>`), time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC), nil
}

type memoryCache struct {
	data     []byte
	metadata CacheMetadata
}

func (c *memoryCache) Read(string) ([]byte, error) {
	if c.data == nil {
		return nil, fs.ErrNotExist
	}
	return c.data, nil
}

func (c *memoryCache) ReadMetadata(string) (CacheMetadata, error) {
	if c.data == nil {
		return CacheMetadata{}, fs.ErrNotExist
	}
	return c.metadata, nil
}

func (c *memoryCache) Write(_ string, data []byte, lastModified time.Time) error {
	c.data = append([]byte(nil), data...)
	c.metadata.LastModified = lastModified
	return nil
}

func (c *memoryCache) Remove(string) error {
	c.data = nil
	return nil
}

func TestServiceCreatesAndCachesFeed(t *testing.T) {
	store, err := storage.NewJSONStore(t.TempDir() + "/feeds.json")
	if err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{}
	service := NewService(store, &memoryCache{}, source, rss.Filter)

	feed, err := service.CreateFeed(context.Background(), "alice", "Foo feed", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	data, err := service.CachedFeed(context.Background(), feed.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Keep foo") || strings.Contains(string(data), "<title>Drop</title>") {
		t.Fatalf("unexpected filtered feed: %s", data)
	}
}

func TestServiceKeepsCacheWhenHeadHasNoLastModified(t *testing.T) {
	store, err := storage.NewJSONStore(t.TempDir() + "/feeds.json")
	if err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{}
	cache := &memoryCache{}
	service := NewService(store, cache, source, rss.Filter)
	feed, err := store.Create("alice", "Foo feed", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.refreshFeed(context.Background(), feed, true); err != nil {
		t.Fatal(err)
	}
	cache.data = []byte("cached")
	cache.metadata.LastModified = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if err := service.refreshFeed(context.Background(), feed, false); err != nil {
		t.Fatal(err)
	}
	if source.getCount.Load() != 1 {
		t.Fatalf("missing Last-Modified caused regeneration: GET=%d", source.getCount.Load())
	}
}

func TestServiceIsolatesFeedsByOwner(t *testing.T) {
	store, err := storage.NewJSONStore(t.TempDir() + "/feeds.json")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &memoryCache{}, &sourceStub{}, rss.Filter)
	feed, err := service.CreateFeed(context.Background(), "alice", "Alice feed", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if feeds := service.ListFeeds("bob"); len(feeds) != 0 {
		t.Fatalf("bob saw alice's feeds: %#v", feeds)
	}
	if err := service.DeleteFeed("bob", feed.Slug); !errors.Is(err, ErrFeedNotFound) {
		t.Fatalf("expected cross-owner delete to be hidden, got %v", err)
	}
	if _, err := service.CachedFeed(context.Background(), feed.Slug); err != nil {
		t.Fatalf("public feed was not readable: %v", err)
	}
}
