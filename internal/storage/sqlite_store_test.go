package storage

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"rss-studio/internal/domain"
	"rss-studio/internal/policy"
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
	if version != 8 {
		t.Fatalf("expected schema version 8, got %d", version)
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
	if _, err := store.Authenticate("missing", "password-1234"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expected missing user to be rejected, got %v", err)
	}
	if _, err := store.CreateUser("", "password-1234"); !errors.Is(err, domain.ErrInvalidUser) {
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

func TestSQLiteStoreRejectsInvalidNewPasswords(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	tests := []struct {
		name     string
		password string
	}{
		{name: "empty", password: ""},
		{name: "short", password: strings.Repeat("a", minimumPasswordRunes-1)},
		{name: "invalid UTF-8", password: string([]byte{'a', 0xff, 'b'})},
		{name: "over bcrypt byte capacity", password: strings.Repeat("a", bcryptMaxPasswordBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.CreateUser(test.name, test.password)
			if !errors.Is(err, domain.ErrInvalidUser) {
				t.Fatalf("expected invalid user error, got %v", err)
			}
			if test.password != "" && err.Error() != "invalid user: password policy" {
				t.Fatalf("expected generic password policy error, got %q", err)
			}
		})
	}
}

func TestSQLiteStoreAcceptsUnicodeAndLongPasswords(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	passwords := []struct {
		username string
		password string
	}{
		{username: "unicode", password: strings.Repeat("界", minimumPasswordRunes)},
		{username: "long", password: strings.Repeat("a", bcryptMaxPasswordBytes)},
	}
	for _, test := range passwords {
		user, err := store.CreateUser(test.username, test.password)
		if err != nil {
			t.Fatalf("create %s user: %v", test.username, err)
		}
		if user.PasswordHash == test.password || strings.Contains(user.PasswordHash, test.password) {
			t.Fatalf("plaintext password was stored for %s user", test.username)
		}
		if _, err := store.Authenticate(test.username, test.password); err != nil {
			t.Fatalf("authenticate %s user: %v", test.username, err)
		}
	}
}

func TestSQLiteStoreAuthenticatesLegacyShortPassword(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	legacyPassword := "short"
	hash, err := bcrypt.GenerateFromPassword([]byte(legacyPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := store.db.Exec(
		"INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)",
		"legacy", string(hash), createdAt,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate("legacy", legacyPassword); err != nil {
		t.Fatalf("legacy short password was rejected: %v", err)
	}
}

func TestSQLiteStoreScopesFeedsByUserAndPersistsPath(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, username := range []string{"alice", "bob"} {
		if _, err := store.CreateUser(username, "password-1234"); err != nil {
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
	for _, path := range []string{aliceFeed.Path, bobFeed.Path} {
		if path == "news-feed" || len(path) != 64 {
			t.Fatalf("expected opaque hashed feed path, got %q", path)
		}
		if decoded, err := hex.DecodeString(path); err != nil || len(decoded) != 32 {
			t.Fatalf("expected 32-byte hexadecimal feed path, got %q", path)
		}
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
	if _, err := store.CreateUser("admin", "password-1234"); err != nil {
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
	if len(feeds) != 1 || feeds[0].Path == "news-feed" || len(feeds[0].Path) != 64 || !feeds[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("unexpected imported feeds: %#v", feeds)
	}
	if decoded, err := hex.DecodeString(feeds[0].Path); err != nil || len(decoded) != 32 {
		t.Fatalf("expected imported feed path to be opaque hash, got %q", feeds[0].Path)
	}
	if feeds[0].Operator != domain.FilterContains {
		t.Fatalf("expected legacy feeds to use contains operator, got %q", feeds[0].Operator)
	}
}

func TestSQLiteStoreServiceTokensAreHashedScopedExpiringAndRevocable(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("alice", "password-1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("bob", "password-1234"); err != nil {
		t.Fatal(err)
	}
	alice, err := store.User("alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.User("bob")
	if err != nil {
		t.Fatal(err)
	}

	expiresAt := time.Now().UTC().Add(time.Hour)
	token, raw, err := store.CreateServiceToken(alice, "agent", []string{domain.ScopeFeedsDelete, domain.ScopeFeedsRead}, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || token.ID == 0 || !token.ExpiresAt.Equal(expiresAt.UTC()) {
		t.Fatalf("unexpected created service token: %#v", token)
	}
	if strings.Contains(token.Name, raw) {
		t.Fatal("raw token leaked into token metadata")
	}
	var storedHash, storedScopes string
	if err := store.db.QueryRow("SELECT token_hash, scopes FROM service_tokens WHERE id = ?", token.ID).Scan(&storedHash, &storedScopes); err != nil {
		t.Fatal(err)
	}
	if storedHash == raw || len(storedHash) != 64 || strings.Contains(storedScopes, raw) {
		t.Fatalf("raw token was persisted: hash=%q scopes=%q", storedHash, storedScopes)
	}

	authenticated, err := store.AuthenticateServiceToken(raw, expiresAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("valid service token rejected: %v", err)
	}
	if authenticated.UserID != alice.ID || authenticated.Username != alice.Username || len(authenticated.Scopes) != 2 {
		t.Fatalf("unexpected authenticated service token: %#v", authenticated)
	}
	if _, err := store.AuthenticateServiceToken(raw, expiresAt); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("expired service token accepted: %v", err)
	}

	aliceTokens, err := store.ListServiceTokens(alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceTokens) != 1 || aliceTokens[0].ID != token.ID || aliceTokens[0].RevokedAt != nil {
		t.Fatalf("unexpected alice token list: %#v", aliceTokens)
	}
	bobTokens, err := store.ListServiceTokens(bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobTokens) != 0 {
		t.Fatalf("bob saw alice token: %#v", bobTokens)
	}
	if err := store.RevokeServiceToken(bob, token.ID); !errors.Is(err, ErrServiceTokenNotFound) {
		t.Fatalf("expected cross-tenant revoke rejection, got %v", err)
	}
	if err := store.RevokeServiceToken(alice, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateServiceToken(raw, expiresAt.Add(-time.Minute)); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("revoked service token accepted: %v", err)
	}
	if err := store.RevokeServiceToken(alice, token.ID); !errors.Is(err, ErrServiceTokenNotFound) {
		t.Fatalf("expected repeated revoke to fail safely, got %v", err)
	}
}

func TestSQLiteStoreCleansTerminalServiceTokensAfterRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss-studio.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	oldExpired, _, err := store.CreateServiceToken(owner, "old-expired", []string{domain.ScopeFeedsRead}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	oldRevoked, _, err := store.CreateServiceToken(owner, "old-revoked", []string{domain.ScopeFeedsRead}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	recentExpired, _, err := store.CreateServiceToken(owner, "recent-expired", []string{domain.ScopeFeedsRead}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	active, _, err := store.CreateServiceToken(owner, "active", []string{domain.ScopeFeedsRead}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE service_tokens SET expires_at = ? WHERE id = ?", now.Add(-serviceTokenRetention-time.Hour).Format(time.RFC3339Nano), oldExpired.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeServiceToken(owner, oldRevoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE service_tokens SET revoked_at = ? WHERE id = ?", now.Add(-serviceTokenRetention-time.Hour).Format(time.RFC3339Nano), oldRevoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE service_tokens SET expires_at = ? WHERE id = ?", now.Add(-time.Hour).Format(time.RFC3339Nano), recentExpired.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM service_tokens WHERE id IN (?, ?)", oldExpired.ID, oldRevoked.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("old terminal service tokens were retained: %d", count)
	}
	var retained int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM service_tokens WHERE id IN (?, ?)", recentExpired.ID, active.ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 2 {
		t.Fatalf("recent terminal or active service token was removed: %d", retained)
	}
}

func TestSQLiteStoreUpgradePreservesUsersAndFeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss-studio.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	feed, err := store.Create(owner, "News", "https://example.test/news.xml", "news")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	for _, statement := range []string{
		"DROP TABLE quota_usage_buckets",
		"DROP TABLE rate_limit_buckets",
		"DROP TABLE idempotency_keys",
		"DROP TABLE service_tokens",
		"DELETE FROM schema_migrations WHERE version >= 4",
	} {
		if _, err := store.db.Exec(statement); err != nil {
			store.Close()
			t.Fatalf("prepare pre-service-token database: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	version, err := upgraded.SchemaVersion()
	if err != nil || version != 8 {
		t.Fatalf("unexpected upgraded schema version: version=%d err=%v", version, err)
	}
	if _, err := upgraded.Authenticate("alice", "password-1234"); err != nil {
		t.Fatalf("existing user was not preserved: %v", err)
	}
	feeds, err := upgraded.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].Path != feed.Path {
		t.Fatalf("existing feed was not preserved: %#v", feeds)
	}
	var serviceTokenTable int
	if err := upgraded.db.QueryRow("SELECT COUNT(*) FROM service_tokens").Scan(&serviceTokenTable); err != nil {
		t.Fatalf("service token table was not recreated: %v", err)
	}
}

func TestSQLiteStoreReclaimsExpiredIdempotencyClaim(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := store.ClaimIdempotency(owner.ID, "credential", "request-key", "first-request", now)
	if err != nil || !first.Acquired || first.ID == 0 {
		t.Fatalf("initial claim failed: claim=%#v err=%v", first, err)
	}
	if err := store.CompleteIdempotency(first.ID, []byte(`{"path":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteIdempotency(first.ID, []byte(`{"path":"first"}`)); err != nil {
		t.Fatalf("idempotent completion was not replay-safe: %v", err)
	}
	if _, err := store.db.Exec("UPDATE idempotency_keys SET expires_at = ? WHERE id = ?", now.Add(-time.Minute).Format(time.RFC3339Nano), first.ID); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := store.ClaimIdempotency(owner.ID, "credential", "request-key", "second-request", now)
	if err != nil || !reclaimed.Acquired || reclaimed.ID == first.ID {
		t.Fatalf("expired claim was not safely reclaimed: claim=%#v err=%v", reclaimed, err)
	}
	if err := store.CompleteIdempotency(reclaimed.ID, []byte(`{"path":"second"}`)); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.ClaimIdempotency(owner.ID, "credential", "request-key", "second-request", now)
	if err != nil || !replayed.Completed || string(replayed.Result) != `{"path":"second"}` {
		t.Fatalf("reclaimed result was not replayed: claim=%#v err=%v", replayed, err)
	}

	var storedKey, storedCredential string
	if err := store.db.QueryRow("SELECT key_hash, credential_hash FROM idempotency_keys WHERE id = ?", reclaimed.ID).Scan(&storedKey, &storedCredential); err != nil {
		t.Fatal(err)
	}
	if storedKey == "request-key" || storedCredential == "credential" {
		t.Fatal("raw idempotency values were persisted")
	}
}

func TestSQLiteStoreDoesNotReclaimExpiredPendingIdempotencyClaim(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := store.ClaimIdempotency(owner.ID, "credential", "request-key", "request", now)
	if err != nil || !first.Acquired {
		t.Fatalf("initial claim failed: claim=%#v err=%v", first, err)
	}
	prepared := []byte(`{"path":"prepared"}`)
	if err := store.PrepareIdempotency(first.ID, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE idempotency_keys SET expires_at = ? WHERE id = ?", now.Add(-time.Minute).Format(time.RFC3339Nano), first.ID); err != nil {
		t.Fatal(err)
	}

	stale, err := store.ClaimIdempotency(owner.ID, "credential", "request-key", "request", now)
	if err != nil || !stale.Pending || !stale.RecoveryRequired || stale.Acquired || string(stale.Result) != string(prepared) {
		t.Fatalf("expired pending claim was reclaimed: claim=%#v err=%v", stale, err)
	}
	if err := store.CompleteIdempotency(stale.ID, stale.Result); err != nil {
		t.Fatalf("prepared pending result was not recoverable: %v", err)
	}
}

func TestSQLiteStoreQuotaBucketsResetRefundAndIsolateAccounts(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alice, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser("bob", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	limits := policy.QuotaLimits{
		MaxFeedsPerAccount:               2,
		PreviewsPerDay:                   1,
		CreationsPerDay:                  2,
		SplitOperationsPerDay:            2,
		MaxConcurrentExpensiveOperations: 1,
	}
	now := time.Date(2026, time.January, 2, 23, 30, 0, 0, time.UTC)
	usage := policy.QuotaUsage{Previews: 1, ConcurrentOperations: 1}
	decision, reservation, err := store.ReserveQuota(alice.ID, usage, limits, now)
	if err != nil || !decision.Allowed {
		t.Fatalf("alice quota reservation failed: decision=%#v err=%v", decision, err)
	}
	if err := store.RefundQuota(policy.QuotaReservation{
		AccountID: reservation.AccountID, PeriodStart: reservation.PeriodStart,
		Usage: policy.QuotaUsage{ConcurrentOperations: 1},
	}); err != nil {
		t.Fatal(err)
	}
	decision, _, err = store.ReserveQuota(alice.ID, usage, limits, now)
	if err != nil || decision.Allowed || decision.Kind != policy.QuotaPreviewsPerDay || decision.ResetAt.IsZero() {
		t.Fatalf("preview quota did not exhaust or expose reset: decision=%#v err=%v", decision, err)
	}

	bobDecision, bobReservation, err := store.ReserveQuota(bob.ID, usage, limits, now)
	if err != nil || !bobDecision.Allowed {
		t.Fatalf("bob inherited alice quota: decision=%#v err=%v", bobDecision, err)
	}
	if err := store.RefundQuota(bobReservation); err != nil {
		t.Fatal(err)
	}

	resetDecision, resetReservation, err := store.ReserveQuota(alice.ID, usage, limits, now.Add(24*time.Hour))
	if err != nil || !resetDecision.Allowed {
		t.Fatalf("daily quota did not reset: decision=%#v err=%v", resetDecision, err)
	}
	if err := store.RefundQuota(resetReservation); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStoreQuotaConcurrentReservationsAreAtomic(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	limits := policy.DefaultQuotaLimits()
	limits.MaxConcurrentExpensiveOperations = 1
	const attempts = 8
	type result struct {
		decision    policy.QuotaDecision
		reservation policy.QuotaReservation
		err         error
	}
	results := make(chan result, attempts)
	start := make(chan struct{})
	now := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < attempts; i++ {
		go func() {
			<-start
			decision, reservation, err := store.ReserveQuota(owner.ID, policy.QuotaUsage{ConcurrentOperations: 1}, limits, now)
			results <- result{decision: decision, reservation: reservation, err: err}
		}()
	}
	close(start)
	allowed := 0
	var held policy.QuotaReservation
	for i := 0; i < attempts; i++ {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.decision.Allowed {
			allowed++
			held = result.reservation
		}
	}
	if allowed != 1 {
		t.Fatalf("concurrent quota reservations allowed %d operations, want 1", allowed)
	}
	if err := store.RefundQuota(held); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStoreBoundsBrowserSessionsWithoutTouchingServiceTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss-studio.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	_, integrationToken, err := store.CreateServiceToken(owner, "integration", []string{domain.ScopeFeedsRead}, time.Now().UTC().Add(time.Hour))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	for i := 0; i < maxBrowserSessions+3; i++ {
		if _, _, err := store.CreateBrowserSession(owner, time.Now().UTC().Add(time.Hour)); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	var browserCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM browser_sessions WHERE user_id = ?", owner.ID).Scan(&browserCount); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if browserCount != maxBrowserSessions {
		store.Close()
		t.Fatalf("browser sessions were not bounded: got %d, want %d", browserCount, maxBrowserSessions)
	}
	if _, err := store.AuthenticateServiceToken(integrationToken, time.Now().UTC()); err != nil {
		store.Close()
		t.Fatalf("valid integration token was affected by browser cleanup: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AuthenticateServiceToken(integrationToken, time.Now().UTC()); err != nil {
		t.Fatalf("valid integration token was not preserved after reopen: %v", err)
	}
}

func TestSQLiteStoreCleansOnlyTerminalBrowserSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss-studio.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldExpired, _, err := store.CreateBrowserSession(owner, now.Add(time.Hour))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	oldRevoked, _, err := store.CreateBrowserSession(owner, now.Add(time.Hour))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	recentRevoked, _, err := store.CreateBrowserSession(owner, now.Add(time.Hour))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	active, _, err := store.CreateBrowserSession(owner, now.Add(time.Hour))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.RevokeBrowserSession(owner, oldRevoked); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.RevokeBrowserSession(owner, recentRevoked); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE browser_sessions SET expires_at = ? WHERE token_hash = ?", now.Add(-browserSessionRetention-time.Hour).Format(time.RFC3339Nano), hashBrowserSession(oldExpired)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE browser_sessions SET revoked_at = ? WHERE token_hash = ?", now.Add(-browserSessionRetention-time.Hour).Format(time.RFC3339Nano), hashBrowserSession(oldRevoked)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM browser_sessions WHERE token_hash IN (?, ?)", hashBrowserSession(oldExpired), hashBrowserSession(oldRevoked)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("old terminal browser sessions were retained: %d", count)
	}
	if _, _, err := store.AuthenticateBrowserSession(active, now); err != nil {
		t.Fatalf("active browser session was removed: %v", err)
	}
	if _, _, err := store.AuthenticateBrowserSession(recentRevoked, now); !errors.Is(err, ErrInvalidBrowserSession) {
		t.Fatalf("recent revoked browser session accepted: %v", err)
	}
}
