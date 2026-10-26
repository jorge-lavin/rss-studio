package httpapi_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/domain"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/policy"
	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
)

type recordingRateLimitStore struct {
	*storage.SQLiteStore
	calls       int
	unavailable bool
}

func (s *recordingRateLimitStore) CheckRateLimits(operation string, limit int, window time.Duration, buckets []policy.RateLimitBucket, now time.Time) (policy.RateLimitDecision, error) {
	s.calls++
	if s.unavailable {
		return policy.RateLimitDecision{}, errors.New("rate limit store unavailable")
	}
	return s.SQLiteStore.CheckRateLimits(operation, limit, window, buckets, now)
}

type publicFeedFixture struct {
	server    *httptest.Server
	store     *storage.SQLiteStore
	rateStore *recordingRateLimitStore
	feedCache *cache.FileCache
	feed      domain.Feed
}

func newPublicFeedFixture(t *testing.T, now func() time.Time) *publicFeedFixture {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateUser("admin", "password-1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	rateStore := &recordingRateLimitStore{SQLiteStore: store}
	feedCache := cache.NewFileCache(t.TempDir())
	feed, err := store.Create(owner, "Public feed", "https://example.test/rss", "foo")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := feedCache.Write(feed, []byte(`<rss><channel><title>Cached</title></channel></rss>`), time.Time{}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		UserStore: store,
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	service := application.NewServiceWithOperator(rateStore, feedCache, sourceStub{}, rss.FilterWithOperator)
	server := httptest.NewServer(httpapi.NewHandlerWithClock(service, now, auth))
	fixture := &publicFeedFixture{server: server, store: store, rateStore: rateStore, feedCache: feedCache, feed: feed}
	t.Cleanup(func() {
		server.Close()
		store.Close()
	})
	return fixture
}

func (f *publicFeedFixture) addFeed(t *testing.T, name string, cached bool) domain.Feed {
	t.Helper()
	owner, err := f.store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	feed, err := f.store.Create(owner, name, "https://example.test/rss", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if cached {
		if err := f.feedCache.Write(feed, []byte(`<rss><channel><title>Cached</title></channel></rss>`), time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	return feed
}

func (f *publicFeedFixture) publicRequest(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(response, request)
	return response
}

func TestPublicFeedRateLimitAllowsRSSBelowLimit(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	fixture := newPublicFeedFixture(t, func() time.Time { return now })
	response := fixture.publicRequest(t, "/feeds/"+fixture.feed.Path+".xml")

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/rss+xml; charset=utf-8" || response.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("unexpected public feed response: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if response.Header().Get("RateLimit-Limit") != "60" || response.Header().Get("RateLimit-Remaining") != "59" || response.Header().Get("X-RateLimit-Remaining") != "59" {
		t.Fatalf("missing public feed rate-limit headers: %v", response.Header())
	}
}

func TestPublicFeedRateLimitRejectsAfterSixtyAndResets(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	fixture := newPublicFeedFixture(t, func() time.Time { return now })
	path := "/feeds/" + fixture.feed.Path + ".xml"
	for attempt := 0; attempt < policy.PublicFeedRateLimit; attempt++ {
		response := fixture.publicRequest(t, path)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d unexpectedly failed: %d %s", attempt, response.Code, response.Body.String())
		}
	}

	response := fixture.publicRequest(t, path)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "60" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected rate-limit response: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	for _, name := range []string{"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
		if response.Header().Get(name) == "" {
			t.Fatalf("rate-limit response missing %s header: %v", name, response.Header())
		}
	}
	body := response.Body.String()
	if strings.Contains(body, fixture.feed.Path) || strings.Contains(body, "https://example.test/rss") || strings.Contains(body, "<rss>") {
		t.Fatalf("rate-limit response exposed feed data: %s", body)
	}

	now = now.Add(policy.RateLimitWindow)
	response = fixture.publicRequest(t, path)
	if response.Code != http.StatusOK || response.Header().Get("RateLimit-Remaining") != "59" {
		t.Fatalf("rate limit did not reset: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestPublicFeedRateLimitBucketsAreIndependentFromOtherFeedsAndAPI(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	fixture := newPublicFeedFixture(t, func() time.Time { return now })
	otherFeed := fixture.addFeed(t, "Other feed", true)
	firstPath := "/feeds/" + fixture.feed.Path + ".xml"
	otherPath := "/feeds/" + otherFeed.Path + ".xml"

	for attempt := 0; attempt < policy.PublicFeedRateLimit; attempt++ {
		response := fixture.publicRequest(t, firstPath)
		if response.Code != http.StatusOK {
			t.Fatalf("first feed attempt %d unexpectedly failed: %d", attempt, response.Code)
		}
	}
	if response := fixture.publicRequest(t, firstPath); response.Code != http.StatusTooManyRequests {
		t.Fatalf("first feed was not limited: %d", response.Code)
	}
	if response := fixture.publicRequest(t, otherPath); response.Code != http.StatusOK || response.Header().Get("RateLimit-Remaining") != "59" {
		t.Fatalf("second feed inherited first feed limit: %d headers=%v", response.Code, response.Header())
	}

	token := loginToken(t, fixture.server)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/feeds", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	fixture.server.Config.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("RateLimit-Limit") != "120" || response.Header().Get("RateLimit-Remaining") != "119" {
		t.Fatalf("public requests consumed authenticated API bucket: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestPublicFeedRateLimitSkipsUnknownAndCacheNotReadyFeeds(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	fixture := newPublicFeedFixture(t, func() time.Time { return now })
	if response := fixture.publicRequest(t, "/feeds/unknown.xml"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown feed returned %d", response.Code)
	}
	notReady := fixture.addFeed(t, "Not ready", false)
	if response := fixture.publicRequest(t, "/feeds/"+notReady.Path+".xml"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cache-not-ready feed returned %d", response.Code)
	}
	if fixture.rateStore.calls != 0 {
		t.Fatalf("unknown or cache-not-ready feeds invoked rate-limit storage %d times", fixture.rateStore.calls)
	}
}

func TestPublicFeedRateLimitFailsClosedWhenStorageUnavailable(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	fixture := newPublicFeedFixture(t, func() time.Time { return now })
	fixture.rateStore.unavailable = true
	path := "/feeds/" + fixture.feed.Path + ".xml"
	response := fixture.publicRequest(t, path)

	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected unavailable response: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), fixture.feed.Path) || strings.Contains(string(body), "https://example.test/rss") || strings.Contains(string(body), "<rss>") {
		t.Fatalf("storage error exposed feed data: %s", body)
	}
}

var _ application.FeedStore = (*recordingRateLimitStore)(nil)
var _ policy.RateLimitStore = (*recordingRateLimitStore)(nil)
