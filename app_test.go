package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateAndServeFeed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(writer, `<rss version="2.0"><channel><title>Source</title><item><title>Keep foo</title><guid>1</guid><enclosure url="https://example.test/1.mp3" type="audio/mpeg"/></item><item><title>Drop</title><guid>2</guid></item></channel></rss>`)
	}))
	defer upstream.Close()

	store, err := newStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := newApp(store, upstream.Client())
	server := httptest.NewServer(app.routes())
	defer server.Close()

	response, err := http.PostForm(server.URL+"/feeds", map[string][]string{
		"name":       {"Foo feed"},
		"source_url": {upstream.URL},
		"keyword":    {"foo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected redirect followed by home page, got %d", response.StatusCode)
	}

	response, err = http.Get(server.URL + "/feeds/foo-feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "Keep foo") || strings.Contains(string(body), "<title>Drop</title>") {
		t.Fatalf("unexpected feed body: %s", body)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/rss+xml") {
		t.Fatalf("unexpected content type %q", contentType)
	}
}

func TestStorePersistsFeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feeds.json")
	first, err := newStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.create("News Feed", "https://example.test/rss", "go"); err != nil {
		t.Fatal(err)
	}

	second, err := newStore(path)
	if err != nil {
		t.Fatal(err)
	}
	feed, found := second.get("news-feed")
	if !found || feed.Keyword != "go" {
		t.Fatalf("persisted feed not found: %#v", feed)
	}
}

func TestRefreshUsesLastModifiedAndServesPrecomputedFeed(t *testing.T) {
	var getCount atomic.Int32
	var headCount atomic.Int32
	lastModified := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Last-Modified", lastModified.Format(http.TimeFormat))
		if request.Method == http.MethodHead {
			headCount.Add(1)
			return
		}
		getCount.Add(1)
		io.WriteString(writer, `<rss version="2.0"><channel><title>Source</title><item><title>Foo</title><guid>1</guid></item></channel></rss>`)
	}))
	defer upstream.Close()

	store, err := newStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	feed, err := store.create("Foo feed", upstream.URL, "foo")
	if err != nil {
		t.Fatal(err)
	}
	app := newApp(store, upstream.Client())
	if err := app.refreshFeed(context.Background(), feed, true); err != nil {
		t.Fatal(err)
	}
	if getCount.Load() != 1 {
		t.Fatalf("expected one initial GET, got %d", getCount.Load())
	}

	request := httptest.NewRequest(http.MethodGet, "/feeds/foo-feed.xml", nil)
	request.SetPathValue("file", "foo-feed.xml")
	response := httptest.NewRecorder()
	app.checkedAt[feed.Slug] = time.Now()
	app.serveFeed(response, request)
	if response.Code != http.StatusOK || getCount.Load() != 1 || headCount.Load() != 0 {
		t.Fatalf("serving cache contacted upstream: status=%d GET=%d HEAD=%d", response.Code, getCount.Load(), headCount.Load())
	}

	if err := app.refreshFeed(context.Background(), feed, false); err != nil {
		t.Fatal(err)
	}
	if getCount.Load() != 1 || headCount.Load() != 1 {
		t.Fatalf("unchanged feed was downloaded: GET=%d HEAD=%d", getCount.Load(), headCount.Load())
	}

	lastModified = lastModified.Add(time.Hour)
	if err := app.refreshFeed(context.Background(), feed, false); err != nil {
		t.Fatal(err)
	}
	if getCount.Load() != 2 || headCount.Load() != 2 {
		t.Fatalf("newer feed was not downloaded: GET=%d HEAD=%d", getCount.Load(), headCount.Load())
	}
}

func TestServeFeedRefreshesInBackground(t *testing.T) {
	headStarted := make(chan struct{}, 1)
	releaseHead := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Last-Modified", time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		if request.Method == http.MethodHead {
			headStarted <- struct{}{}
			<-releaseHead
			return
		}
		io.WriteString(writer, `<rss version="2.0"><channel><title>Source</title><item><title>Foo</title><guid>1</guid></item></channel></rss>`)
	}))
	defer upstream.Close()

	store, err := newStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	feed, err := store.create("Foo feed", upstream.URL, "foo")
	if err != nil {
		t.Fatal(err)
	}
	app := newApp(store, upstream.Client())
	if err := app.refreshFeed(context.Background(), feed, true); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/feeds/foo-feed.xml", nil)
	request.SetPathValue("file", "foo-feed.xml")
	response := httptest.NewRecorder()
	started := time.Now()
	app.serveFeed(response, request)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cached response waited for refresh: %s", elapsed)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("expected cached response, got %d", response.Code)
	}

	select {
	case <-headStarted:
	case <-time.After(time.Second):
		t.Fatal("background HEAD request did not start")
	}
	close(releaseHead)
}

func TestMissingHeadLastModifiedKeepsCachedFeed(t *testing.T) {
	var getCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead {
			return
		}
		getCount.Add(1)
		writer.Header().Set("Last-Modified", time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		io.WriteString(writer, `<rss version="2.0"><channel><title>Source</title><item><title>Foo</title><guid>1</guid></item></channel></rss>`)
	}))
	defer upstream.Close()

	store, err := newStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	feed, err := store.create("Foo feed", upstream.URL, "foo")
	if err != nil {
		t.Fatal(err)
	}
	app := newApp(store, upstream.Client())
	if err := app.refreshFeed(context.Background(), feed, true); err != nil {
		t.Fatal(err)
	}
	if err := app.refreshFeed(context.Background(), feed, false); err != nil {
		t.Fatal(err)
	}
	if getCount.Load() != 1 {
		t.Fatalf("missing Last-Modified caused regeneration: GET=%d", getCount.Load())
	}
}
