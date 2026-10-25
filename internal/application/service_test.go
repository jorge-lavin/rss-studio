package application

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rss-studio/internal/domain"
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

func (c *memoryCache) Read(domain.Feed) ([]byte, error) {
	if c.data == nil {
		return nil, fs.ErrNotExist
	}
	return c.data, nil
}

func (c *memoryCache) ReadMetadata(domain.Feed) (CacheMetadata, error) {
	if c.data == nil {
		return CacheMetadata{}, fs.ErrNotExist
	}
	return c.metadata, nil
}

func (c *memoryCache) Write(_ domain.Feed, data []byte, lastModified time.Time) error {
	c.data = append([]byte(nil), data...)
	c.metadata.LastModified = lastModified
	return nil
}

func (c *memoryCache) Remove(domain.Feed) error {
	c.data = nil
	return nil
}

func TestServiceCreatesAndCachesFeed(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &sourceStub{}
	service := NewService(store, &memoryCache{}, source, rss.Filter)
	feed, err := service.CreateFeed(context.Background(), owner, "Foo feed", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	data, err := service.CachedFeed(context.Background(), feed.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Keep foo") || strings.Contains(string(data), "<title>Drop</title>") {
		t.Fatalf("unexpected filtered feed: %s", data)
	}
}

func TestServiceKeepsCacheWhenHeadHasNoLastModified(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &sourceStub{}
	cache := &memoryCache{}
	service := NewService(store, cache, source, rss.Filter)
	feed, err := store.Create(owner, "Foo feed", "https://example.test/rss", "foo")
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

func testSQLiteStore(t *testing.T) (*storage.SQLiteStore, domain.User) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	return store, owner
}
