package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
)

type sourceStub struct{}

func (sourceStub) Head(context.Context, string) (time.Time, error) { return time.Time{}, nil }

func (sourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	return []byte(`<rss version="2.0"><channel><title>Source</title><item><title>Keep foo</title><guid>1</guid></item><item><title>Drop</title><guid>2</guid></item></channel></rss>`), time.Time{}, nil
}

func TestHandlerCreatesAndServesFeed(t *testing.T) {
	store, err := storage.NewJSONStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewService(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.Filter)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()

	response, err := http.PostForm(server.URL+"/feeds", map[string][]string{
		"name":       {"Foo feed"},
		"source_url": {"https://example.test/rss"},
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
}
