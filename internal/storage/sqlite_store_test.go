package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rss-studio/internal/domain"
)

func TestSQLiteStoreMigratesAndPersistsMultipleUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss-studio.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("expected schema version 3, got %d", version)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("rerunning migrations: %v", err)
	}
	admin, err := store.CreateUser(" admin ", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if admin.Username != "admin" || admin.PasswordHash == "correct horse" {
		t.Fatalf("user credentials were not normalized and hashed: %#v", admin)
	}
	if _, err := store.CreateUser("reader", "reader password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("admin", "new password"); !errors.Is(err, domain.ErrUsernameTaken) {
		t.Fatalf("expected duplicate username to be rejected, got %v", err)
	}
	if _, err := store.Authenticate("admin", "correct horse"); err != nil {
		t.Fatalf("original bootstrap password rejected: %v", err)
	}
	if _, err := store.Authenticate("admin", "new password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expected replacement password to be rejected, got %v", err)
	}
	if _, err := store.Authenticate("missing", "password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expected missing user to be rejected, got %v", err)
	}
	if _, err := store.CreateUser("", "password"); !errors.Is(err, domain.ErrInvalidUser) {
		t.Fatalf("expected empty username to be rejected, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("expected database permissions 0600, got %04o", permissions)
	}

	second, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Authenticate("admin", "correct horse"); err != nil {
		t.Fatalf("persisted user rejected: %v", err)
	}
	if _, err := second.Authenticate("reader", "reader password"); err != nil {
		t.Fatalf("second persisted user rejected: %v", err)
	}
}

func TestSQLiteStoreScopesFeedsByUserAndPersistsPath(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, username := range []string{"alice", "bob"} {
		if _, err := store.CreateUser(username, "password"); err != nil {
			t.Fatal(err)
		}
	}
	alice, err := store.User("alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.User("bob")
	if err != nil {
		t.Fatal(err)
	}

	aliceFeed, err := store.CreateWithOperator(alice, "News Feed", "https://example.test/alice.xml", domain.FilterStartsWith, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bobFeed, err := store.Create(bob, "News Feed", "https://example.test/bob.xml", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if aliceFeed.Path == bobFeed.Path {
		t.Fatalf("expected globally unique paths, got %q", aliceFeed.Path)
	}

	aliceFeeds, err := store.List(alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceFeeds) != 1 || aliceFeeds[0].Path != aliceFeed.Path || aliceFeeds[0].TenantID != alice.ID {
		t.Fatalf("unexpected alice feeds: %#v", aliceFeeds)
	}
	bobFeeds, err := store.List(bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobFeeds) != 1 || bobFeeds[0].Path != bobFeed.Path || bobFeeds[0].TenantID != bob.ID {
		t.Fatalf("unexpected bob feeds: %#v", bobFeeds)
	}

	if err := store.Delete(alice, bobFeed.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected cross-tenant delete to fail, got %v", err)
	}
	stored, found, err := store.Get(aliceFeed.Path)
	if err != nil || !found {
		t.Fatalf("stored feed not found: found=%v err=%v", found, err)
	}
	if stored.Path != aliceFeed.Path || stored.SourceURL != aliceFeed.SourceURL || stored.Operator != domain.FilterStartsWith {
		t.Fatalf("unexpected stored feed: %#v", stored)
	}
}

func TestSQLiteStoreImportsAndRemovesLegacyFeedStore(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, "feeds.json")
	createdAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	data, err := json.Marshal([]legacyFeedRecord{{
		Name:      "News Feed",
		Slug:      "news-feed",
		SourceURL: "https://example.test/rss",
		Keyword:   "news",
		CreatedAt: createdAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(filepath.Join(root, "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportLegacyFeeds(legacyPath, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy feed store still exists: %v", err)
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].Path != "news-feed" || !feeds[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("unexpected imported feeds: %#v", feeds)
	}
	if feeds[0].Operator != domain.FilterContains {
		t.Fatalf("expected legacy feeds to use contains operator, got %q", feeds[0].Operator)
	}
}
