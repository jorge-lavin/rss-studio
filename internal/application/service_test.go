package application

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/idempotency"
	"rss-studio/internal/observability"
	"rss-studio/internal/policy"
	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
)

type sourceStub struct {
	getCount  atomic.Int32
	headCount atomic.Int32
	head      time.Time
}

type auditEventRecorder struct {
	events []observability.Event
}

func (r *auditEventRecorder) Record(event observability.Event) {
	r.events = append(r.events, event)
}

type failingAuditSource struct{}

type idempotencyStoreStub struct {
	*storage.SQLiteStore
	claim            idempotency.Claim
	completeFailures int
	completeCalls    int
	abandonCalls     int
}

func (s *idempotencyStoreStub) ClaimIdempotency(int64, string, string, string, time.Time) (idempotency.Claim, error) {
	if s.claim.ID == 0 {
		s.claim = idempotency.Claim{ID: 1, Pending: true}
		return idempotency.Claim{ID: 1, Acquired: true}, nil
	}
	return s.claim, nil
}

func (s *idempotencyStoreStub) PrepareIdempotency(_ int64, result []byte) error {
	s.claim.Result = append([]byte(nil), result...)
	return nil
}

func (s *idempotencyStoreStub) CompleteIdempotency(id int64, result []byte) error {
	s.completeCalls++
	if s.completeCalls <= s.completeFailures {
		return errors.New("completion unavailable")
	}
	s.claim = idempotency.Claim{ID: id, Completed: true, Result: append([]byte(nil), result...)}
	return nil
}

func (s *idempotencyStoreStub) AbandonIdempotency(int64) error {
	s.abandonCalls++
	return nil
}

func (failingAuditSource) Head(context.Context, string) (time.Time, error) {
	return time.Time{}, errors.New("upstream unavailable")
}

func (failingAuditSource) Get(context.Context, string) ([]byte, time.Time, error) {
	return nil, time.Time{}, errors.New("upstream unavailable")
}

func (s *sourceStub) Head(context.Context, string) (time.Time, error) {
	s.headCount.Add(1)
	return s.head, nil
}

func (s *sourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	s.getCount.Add(1)
	return []byte(`<rss version="2.0"><channel><title>Source</title><item><title>Keep foo</title><guid>1</guid></item><item><title>Drop</title><guid>2</guid></item></channel></rss>`), time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC), nil
}

func TestExecuteIdempotentRetriesCompletionWithoutReexecuting(t *testing.T) {
	store := &idempotencyStoreStub{completeFailures: 1}
	service := NewService(store, nil, nil, nil)
	calls := 0
	operation := func() (any, error) {
		calls++
		return map[string]string{"path": "feed"}, nil
	}

	result, err := service.ExecuteIdempotent(context.Background(), domain.User{ID: 1}, "credential", "key", "request", operation)
	if err != nil || string(result) != `{"path":"feed"}` {
		t.Fatalf("completion retry failed: result=%s err=%v", result, err)
	}
	if calls != 1 || store.completeCalls != 2 {
		t.Fatalf("mutation or completion was retried incorrectly: operations=%d completions=%d", calls, store.completeCalls)
	}

	replayed, err := service.ExecuteIdempotent(context.Background(), domain.User{ID: 1}, "credential", "key", "request", operation)
	if err != nil || string(replayed) != string(result) || calls != 1 {
		t.Fatalf("successful completion was not replayed: result=%s err=%v operations=%d", replayed, err, calls)
	}
}

func TestExecuteIdempotentRetainsClaimAfterCompletionFailure(t *testing.T) {
	store := &idempotencyStoreStub{completeFailures: 4}
	service := NewService(store, nil, nil, nil)
	calls := 0
	operation := func() (any, error) {
		calls++
		return map[string]string{"path": "feed"}, nil
	}

	if _, err := service.ExecuteIdempotent(context.Background(), domain.User{ID: 1}, "credential", "key", "request", operation); !errors.Is(err, ErrIdempotencyUnavailable) {
		t.Fatalf("expected unavailable completion error, got %v", err)
	}
	if calls != 1 || store.abandonCalls != 0 {
		t.Fatalf("completion failure allowed unsafe recovery: operations=%d abandons=%d", calls, store.abandonCalls)
	}

	if _, err := service.ExecuteIdempotent(context.Background(), domain.User{ID: 1}, "credential", "key", "request", operation); !errors.Is(err, ErrIdempotencyUnavailable) {
		t.Fatalf("expected unavailable recovery error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("pending claim reran mutation: operations=%d", calls)
	}
}

type memoryCache struct {
	data     []byte
	metadata CacheMetadata
}

type splitMemoryCache struct {
	data           map[string][]byte
	metadata       map[string]CacheMetadata
	writes         int
	failOnWrite    int
	metadataErr    error
	removeFailures int
}

func newSplitMemoryCache() *splitMemoryCache {
	return &splitMemoryCache{data: make(map[string][]byte), metadata: make(map[string]CacheMetadata)}
}

func (c *splitMemoryCache) Read(feed domain.Feed) ([]byte, error) {
	data, ok := c.data[feed.Path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (c *splitMemoryCache) ReadMetadata(feed domain.Feed) (CacheMetadata, error) {
	if c.metadataErr != nil {
		return CacheMetadata{}, c.metadataErr
	}
	metadata, ok := c.metadata[feed.Path]
	if !ok {
		return CacheMetadata{}, fs.ErrNotExist
	}
	return metadata, nil
}

func (c *splitMemoryCache) Write(feed domain.Feed, data []byte, lastModified time.Time) error {
	c.writes++
	if c.failOnWrite > 0 && c.writes == c.failOnWrite {
		return errors.New("cache write failed")
	}
	c.data[feed.Path] = append([]byte(nil), data...)
	c.metadata[feed.Path] = CacheMetadata{LastModified: lastModified}
	return nil
}

func (c *splitMemoryCache) Remove(feed domain.Feed) error {
	if c.removeFailures > 0 {
		c.removeFailures--
		return errors.New("cache remove failed")
	}
	delete(c.data, feed.Path)
	delete(c.metadata, feed.Path)
	return nil
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

func TestServiceReportsRefreshStatus(t *testing.T) {
	service := NewService(nil, nil, nil, nil)
	feed := domain.Feed{TenantID: 1, SourceURL: "https://example.test/rss"}
	if got := service.RefreshStatus(feed); got != RefreshStatusReady {
		t.Fatalf("unexpected initial refresh status: %q", got)
	}

	service.stateMu.Lock()
	service.refreshing[refreshKey{tenantID: feed.TenantID, sourceURL: feed.SourceURL}] = true
	service.stateMu.Unlock()
	if got := service.RefreshStatus(feed); got != RefreshStatusRefreshing {
		t.Fatalf("unexpected active refresh status: %q", got)
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

func TestServicePreviewsWithoutPersisting(t *testing.T) {
	source := &previewSourceStub{}
	service := NewService(nil, nil, source, rss.Filter)

	results, err := service.Preview(context.Background(), "https://example.test/rss", []PreviewRule{
		{Name: "AI", Keyword: "ai"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.getCount != 1 {
		t.Fatalf("expected one source fetch, got %d", source.getCount)
	}
	if len(results) != 1 || results[0].Name != "AI" || results[0].Keyword != "ai" || results[0].Count != 6 {
		t.Fatalf("unexpected preview result: %#v", results)
	}
	if len(results[0].Titles) != 5 {
		t.Fatalf("expected five sample titles, got %d", len(results[0].Titles))
	}
	if results[0].Titles[0] != "AI episode 1" || results[0].Titles[4] != "AI episode 5" {
		t.Fatalf("unexpected sample titles: %#v", results[0].Titles)
	}
	if results[0].FeedImageURL != "https://example.test/show.jpg" || results[0].Episodes[0].ImageURL != "https://example.test/episodes/1.jpg" {
		t.Fatalf("unexpected preview image metadata: %#v", results[0])
	}
}

func TestPreviewImageURLOnlyReturnsHTTPImages(t *testing.T) {
	base := "https://example.test/path/feed.xml"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "relative", in: "../art/show.jpg", want: "https://example.test/art/show.jpg"},
		{name: "https", in: "https://images.test/show.jpg", want: "https://images.test/show.jpg"},
		{name: "javascript", in: "javascript:alert(1)"},
		{name: "userinfo", in: "https://user:pass@images.test/show.jpg"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := previewImageURL(test.in, base); got != test.want {
				t.Fatalf("previewImageURL(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestServiceAuditEventsAttributeQuotaAndUpstreamOutcomes(t *testing.T) {
	store, owner := testSQLiteStore(t)
	recorder := &auditEventRecorder{}
	identity := policy.WithIdentity(context.Background(), policy.Identity{
		User:    owner,
		TokenID: 23,
	})
	ctx := observability.WithRecorder(identity, recorder)
	limits := policy.DefaultQuotaLimits()
	limits.PreviewsPerDay = 0
	service := NewServiceWithQuotas(store, &memoryCache{}, &sourceStub{}, rss.Filter, limits)

	if _, err := service.PreviewForOwner(ctx, owner, "https://example.test/rss", []PreviewRule{{Name: "Foo", Keyword: "foo"}}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected quota rejection, got %v", err)
	}
	if len(recorder.events) != 1 || recorder.events[0].ActorID != owner.ID || recorder.events[0].TokenID != 23 || !recorder.events[0].QuotaRejected || recorder.events[0].Result != "quota_rejected" {
		t.Fatalf("unexpected quota audit event: %#v", recorder.events)
	}

	recorder.events = nil
	service = NewService(store, &memoryCache{}, failingAuditSource{}, rss.Filter)
	if _, err := service.CreateFeed(ctx, owner, "Foo", "https://example.test/rss", "foo"); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("expected upstream failure, got %v", err)
	}
	if len(recorder.events) != 1 || recorder.events[0].Operation != policy.OperationFeedsCreate || recorder.events[0].ActorID != owner.ID || recorder.events[0].TokenID != 23 || !recorder.events[0].UpstreamFailure || recorder.events[0].Result != "upstream_failure" {
		t.Fatalf("unexpected upstream audit event: %#v", recorder.events)
	}
}

func TestServiceCreatesSplitFeedsWithOneFetchAndSeparateCaches(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &sourceStub{}
	cache := newSplitMemoryCache()
	service := NewService(store, cache, source, rss.Filter)

	feeds, err := service.CreateSplitFeeds(context.Background(), owner, "https://example.test/rss", []PreviewRule{
		{Name: "Foo", Keyword: "foo"},
		{Name: "Keep", Keyword: "keep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.getCount.Load() != 1 {
		t.Fatalf("expected one source fetch, got %d", source.getCount.Load())
	}
	if len(feeds) != 2 || feeds[0].Name != "Foo" || feeds[1].Name != "Keep" {
		t.Fatalf("unexpected split feeds: %#v", feeds)
	}
	foo, err := cache.Read(feeds[0])
	if err != nil {
		t.Fatal(err)
	}
	keep, err := cache.Read(feeds[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(foo) == string(keep) || !strings.Contains(string(foo), "<title>Foo</title>") || !strings.Contains(string(keep), "<title>Keep</title>") {
		t.Fatalf("split caches were not separate: foo=%s keep=%s", foo, keep)
	}
}

func TestServiceCreatesSplitFeedsWithDifferentOperators(t *testing.T) {
	store, owner := testSQLiteStore(t)
	cache := newSplitMemoryCache()
	service := NewServiceWithOperator(store, cache, &sourceStub{}, rss.FilterWithOperator)

	feeds, err := service.CreateSplitFeeds(context.Background(), owner, "https://example.test/rss", []PreviewRule{
		{Name: "Keep", Operator: domain.FilterStartsWith, Keyword: "keep"},
		{Name: "Drop", Operator: domain.FilterEquals, Keyword: "drop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 2 || feeds[0].Operator != domain.FilterStartsWith || feeds[1].Operator != domain.FilterEquals {
		t.Fatalf("operators were not persisted: %#v", feeds)
	}
	for _, feed := range feeds {
		data, err := cache.Read(feed)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "<title>"+feed.Name+"</title>") || !strings.Contains(string(data), "<item>") {
			t.Fatalf("unexpected filtered %s feed: %s", feed.Name, data)
		}
	}
}

func TestServiceRejectsOversizedSplitAggregateBeforeFiltering(t *testing.T) {
	source := &sizedSourceStub{data: bytes.Repeat([]byte("x"), MaxSplitAggregateBytes/4+1)}
	service := NewService(nil, nil, source, rss.Filter)

	_, err := service.CreateSplitFeeds(context.Background(), domain.User{ID: 1}, "https://example.test/rss", []PreviewRule{
		{Name: "One", Keyword: "one"},
		{Name: "Two", Keyword: "two"},
		{Name: "Three", Keyword: "three"},
		{Name: "Four", Keyword: "four"},
	})
	if !errors.Is(err, ErrInvalidFeed) || err.Error() != "invalid feed: split output size limit exceeded" {
		t.Fatalf("expected safe aggregate limit error, got %v", err)
	}
}

func TestServiceRefreshesSameSourceGroupOnce(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &sourceStub{head: time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)}
	cache := newSplitMemoryCache()
	service := NewService(store, cache, source, rss.Filter)

	foo, err := store.Create(owner, "Foo", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	keep, err := store.Create(owner, "Keep", "https://example.test/rss", "keep")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.refreshFeedGroup(context.Background(), foo); err != nil {
		t.Fatal(err)
	}
	if source.headCount.Load() != 1 || source.getCount.Load() != 1 {
		t.Fatalf("expected one grouped HEAD/GET, got HEAD=%d GET=%d", source.headCount.Load(), source.getCount.Load())
	}
	fooData, err := cache.Read(foo)
	if err != nil {
		t.Fatal(err)
	}
	keepData, err := cache.Read(keep)
	if err != nil {
		t.Fatal(err)
	}
	if string(fooData) == string(keepData) || !strings.Contains(string(fooData), "<title>Foo</title>") || !strings.Contains(string(keepData), "<title>Keep</title>") {
		t.Fatalf("group caches were not separate: foo=%s keep=%s", fooData, keepData)
	}
}

func TestServiceGroupedRefreshIgnoresStaleTrigger(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &sourceStub{}
	service := NewService(store, newSplitMemoryCache(), source, rss.Filter)
	feed, err := store.Create(owner, "Foo", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}

	stale := feed
	stale.ID++
	if err := service.refreshFeedGroup(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if source.headCount.Load() != 0 || source.getCount.Load() != 0 {
		t.Fatalf("stale trigger refreshed source: HEAD=%d GET=%d", source.headCount.Load(), source.getCount.Load())
	}

	missing := feed
	missing.Path = "deleted-feed"
	if err := service.refreshFeedGroup(context.Background(), missing); err != nil {
		t.Fatal(err)
	}
	if source.headCount.Load() != 0 || source.getCount.Load() != 0 {
		t.Fatalf("missing trigger refreshed source: HEAD=%d GET=%d", source.headCount.Load(), source.getCount.Load())
	}
}

func TestServiceGroupedRefreshRegeneratesMissingCacheWithoutLastModified(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &sourceStub{}
	cache := newSplitMemoryCache()
	service := NewService(store, cache, source, rss.Filter)

	foo, err := store.Create(owner, "Foo", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	keep, err := store.Create(owner, "Keep", "https://example.test/rss", "keep")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.refreshFeedGroup(context.Background(), foo); err != nil {
		t.Fatal(err)
	}
	if err := cache.Remove(keep); err != nil {
		t.Fatal(err)
	}
	source.headCount.Store(0)
	source.getCount.Store(0)
	if err := service.refreshFeedGroup(context.Background(), foo); err != nil {
		t.Fatal(err)
	}
	if source.headCount.Load() != 1 || source.getCount.Load() != 1 {
		t.Fatalf("expected missing cache to cause one grouped HEAD/GET, got HEAD=%d GET=%d", source.headCount.Load(), source.getCount.Load())
	}
	if _, err := cache.Read(keep); err != nil {
		t.Fatalf("missing grouped cache was not regenerated: %v", err)
	}
}

func TestServiceGroupedRefreshRegeneratesMalformedMetadata(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &sourceStub{}
	cache := newSplitMemoryCache()
	service := NewService(store, cache, source, rss.Filter)

	foo, err := store.Create(owner, "Foo", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.refreshFeedGroup(context.Background(), foo); err != nil {
		t.Fatal(err)
	}
	cache.metadataErr = errors.New("invalid character")
	source.headCount.Store(0)
	source.getCount.Store(0)
	if err := service.refreshFeedGroup(context.Background(), foo); err != nil {
		t.Fatal(err)
	}
	if source.headCount.Load() != 1 || source.getCount.Load() != 1 {
		t.Fatalf("expected malformed metadata to regenerate, got HEAD=%d GET=%d", source.headCount.Load(), source.getCount.Load())
	}
}

func TestServiceRollsBackSplitFeedsAfterCacheFailure(t *testing.T) {
	store, owner := testSQLiteStore(t)
	cache := newSplitMemoryCache()
	cache.failOnWrite = 2
	service := NewService(store, cache, &sourceStub{}, rss.Filter)

	if _, err := service.CreateSplitFeeds(context.Background(), owner, "https://example.test/rss", []PreviewRule{
		{Name: "Foo", Keyword: "foo"},
		{Name: "Keep", Keyword: "keep"},
	}); err == nil {
		t.Fatal("expected cache failure")
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 0 || len(cache.data) != 0 {
		t.Fatalf("split rollback left state: feeds=%#v cache=%#v", feeds, cache.data)
	}
}

func TestServiceReportsSplitRollbackFailure(t *testing.T) {
	store, owner := testSQLiteStore(t)
	cache := newSplitMemoryCache()
	cache.failOnWrite = 2
	cache.removeFailures = 1
	service := NewService(store, cache, &sourceStub{}, rss.Filter)

	_, err := service.CreateSplitFeeds(context.Background(), owner, "https://example.test/rss", []PreviewRule{
		{Name: "Foo", Keyword: "foo"},
		{Name: "Keep", Keyword: "keep"},
	})
	if err == nil || !strings.Contains(err.Error(), "split rollback failed") || !strings.Contains(err.Error(), "cache remove failed") {
		t.Fatalf("expected rollback failure in returned error, got %v", err)
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 0 {
		t.Fatalf("rollback stopped after failure: feeds=%#v", feeds)
	}
}

func TestServicePreviewValidatesBeforeFetching(t *testing.T) {
	source := &previewSourceStub{}
	service := NewService(nil, nil, source, rss.Filter)

	if _, err := service.Preview(context.Background(), "not-a-url", []PreviewRule{{Name: "AI", Keyword: "ai"}}); err == nil {
		t.Fatal("expected invalid source URL to fail")
	}
	if _, err := service.Preview(context.Background(), "https://example.test/rss", nil); err == nil {
		t.Fatal("expected missing outputs to fail")
	}
	if source.getCount != 0 {
		t.Fatalf("validation fetched source %d times", source.getCount)
	}

	tooMany := make([]PreviewRule, MaxSplitOutputs+1)
	for i := range tooMany {
		tooMany[i] = PreviewRule{Name: "Output", Keyword: "keyword"}
	}
	if _, err := service.Preview(context.Background(), "https://example.test/rss", tooMany); !errors.Is(err, ErrInvalidFeed) {
		t.Fatalf("expected preview output limit error, got %v", err)
	}
	if _, err := service.CreateSplitFeeds(context.Background(), domain.User{ID: 1}, "https://example.test/rss", tooMany); !errors.Is(err, ErrInvalidFeed) {
		t.Fatalf("expected split output limit error, got %v", err)
	}
	if source.getCount != 0 {
		t.Fatalf("output limit fetched source %d times", source.getCount)
	}
}

func TestServiceQuotaValidatesBeforeClaimingAndRefundsUpstreamFailure(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &quotaSourceStub{}
	source.fail.Store(true)
	limits := DefaultQuotaConfig()
	limits.PreviewsPerDay = 1
	limits.MaxConcurrentExpensiveOperations = 1
	service := NewServiceWithQuotas(store, nil, source, rss.Filter, limits)

	if _, err := service.PreviewForOwner(context.Background(), owner, "not-a-url", []PreviewRule{{Name: "Foo", Keyword: "foo"}}); !errors.Is(err, ErrInvalidFeed) {
		t.Fatalf("expected validation failure, got %v", err)
	}
	if source.getCount.Load() != 0 {
		t.Fatalf("validation claimed or fetched source: %d", source.getCount.Load())
	}
	if _, err := service.PreviewForOwner(context.Background(), owner, "https://example.test/rss", []PreviewRule{{Name: "Foo", Keyword: "foo"}}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("expected upstream failure, got %v", err)
	}
	source.fail.Store(false)
	if _, err := service.PreviewForOwner(context.Background(), owner, "https://example.test/rss", []PreviewRule{{Name: "Foo", Keyword: "foo"}}); err != nil {
		t.Fatalf("refunded preview quota was not reusable: %v", err)
	}
	if _, err := service.PreviewForOwner(context.Background(), owner, "https://example.test/rss", []PreviewRule{{Name: "Foo", Keyword: "foo"}}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("successful preview did not consume quota: %v", err)
	}
}

func TestServiceQuotaLimitsConcurrentExpensiveOperationsPerAccount(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &quotaSourceStub{started: make(chan struct{}, 1), release: make(chan struct{})}
	limits := DefaultQuotaConfig()
	limits.MaxConcurrentExpensiveOperations = 1
	service := NewServiceWithQuotas(store, nil, source, rss.Filter, limits)
	rules := []PreviewRule{{Name: "Foo", Keyword: "foo"}}
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.PreviewForOwner(context.Background(), owner, "https://example.test/rss", rules)
		firstDone <- err
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("first preview did not reach source")
	}
	if _, err := service.PreviewForOwner(context.Background(), owner, "https://example.test/rss", rules); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("concurrent preview was not rejected: %v", err)
	}
	close(source.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first preview failed: %v", err)
	}
}

func TestServiceFailedRefreshCanRetryImmediately(t *testing.T) {
	store, owner := testSQLiteStore(t)
	source := &failingRefreshSource{getStarted: make(chan struct{}, 2)}
	service := NewService(store, newSplitMemoryCache(), source, rss.Filter)
	feed, err := store.Create(owner, "Foo", "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}

	key := refreshKey{tenantID: feed.TenantID, sourceURL: feed.SourceURL}
	service.triggerRefresh(feed)
	<-source.getStarted
	waitForRefreshIdle(t, service, key)

	service.stateMu.Lock()
	_, checked := service.checkedAt[key]
	service.stateMu.Unlock()
	if checked {
		t.Fatal("failed refresh retained successful-check timestamp")
	}

	service.triggerRefresh(feed)
	select {
	case <-source.getStarted:
	case <-time.After(time.Second):
		t.Fatal("failed refresh did not retry")
	}
	waitForRefreshIdle(t, service, key)
}

func waitForRefreshIdle(t *testing.T, service *Service, key refreshKey) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		service.stateMu.Lock()
		idle := !service.refreshing[key]
		service.stateMu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not finish")
		}
		runtime.Gosched()
	}
}

type previewSourceStub struct {
	getCount int
}

type failingRefreshSource struct {
	getStarted chan struct{}
}

type sizedSourceStub struct {
	data []byte
}

type quotaSourceStub struct {
	getCount atomic.Int32
	fail     atomic.Bool
	started  chan struct{}
	release  chan struct{}
}

func (s *sizedSourceStub) Head(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func (s *sizedSourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	return s.data, time.Time{}, nil
}

func (s *quotaSourceStub) Head(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func (s *quotaSourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	s.getCount.Add(1)
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.release != nil {
		<-s.release
	}
	if s.fail.Load() {
		return nil, time.Time{}, errors.New("source unavailable")
	}
	return []byte(`<rss version="2.0"><channel><title>Source</title><item><title>Keep foo</title></item></channel></rss>`), time.Time{}, nil
}

func (s *failingRefreshSource) Head(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func (s *failingRefreshSource) Get(context.Context, string) ([]byte, time.Time, error) {
	s.getStarted <- struct{}{}
	return nil, time.Time{}, errors.New("source unavailable")
}

func (s *previewSourceStub) Head(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func (s *previewSourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	s.getCount++
	return []byte(`<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>Source</title><itunes:image href="/show.jpg"/><item><title>AI episode 1</title><itunes:image href="episodes/1.jpg"/></item><item><title>AI episode 2</title></item><item><title>AI episode 3</title></item><item><title>AI episode 4</title></item><item><title>AI episode 5</title></item><item><title>AI episode 6</title></item><item><title>Other</title></item></channel></rss>`), time.Time{}, nil
}

func testSQLiteStore(t *testing.T) (*storage.SQLiteStore, domain.User) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.CreateUser("admin", "password-1234"); err != nil {
		t.Fatal(err)
	}
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	return store, owner
}
