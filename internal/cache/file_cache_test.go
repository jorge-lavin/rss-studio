package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rss-studio/internal/domain"
)

func TestFileCacheScopesIdenticalPathsByTenant(t *testing.T) {
	root := t.TempDir()
	cache := NewFileCache(root)
	alice := domain.Feed{TenantID: 1, Path: "news"}
	bob := domain.Feed{TenantID: 2, Path: "news"}
	if err := cache.Write(alice, []byte("alice"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Write(bob, []byte("bob"), time.Time{}); err != nil {
		t.Fatal(err)
	}

	data, err := cache.Read(alice)
	if err != nil || string(data) != "alice" {
		t.Fatalf("unexpected alice cache: %q, %v", data, err)
	}
	data, err = cache.Read(bob)
	if err != nil || string(data) != "bob" {
		t.Fatalf("unexpected bob cache: %q, %v", data, err)
	}
	for _, tenant := range []string{"1", "2"} {
		if _, err := os.Stat(filepath.Join(root, tenant, "news.xml")); err != nil {
			t.Fatalf("tenant cache path missing for %s: %v", tenant, err)
		}
	}
}

func TestFileCacheRejectsUnsafeFeedPath(t *testing.T) {
	cache := NewFileCache(t.TempDir())
	_, err := cache.Read(domain.Feed{TenantID: 1, Path: "../outside"})
	if err == nil {
		t.Fatal("expected unsafe feed path to fail")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unsafe path reached filesystem: %v", err)
	}
}
