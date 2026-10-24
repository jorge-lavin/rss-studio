package httpapi_test

import (
	"context"
	"encoding/json"
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
	"rss-studio/internal/version"
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
	homeBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected redirect followed by home page, got %d", response.StatusCode)
	}
	if !strings.Contains(string(homeBody), "RSS Studio · Version "+version.Version) {
		t.Fatalf("home page does not show version %q: %s", version.Version, homeBody)
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

func TestAuthenticatedHandlerProtectsManagementAndLeavesFeedsPublic(t *testing.T) {
	store, err := storage.NewJSONStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewService(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.Filter)
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		Username:  "admin",
		Password:  "password",
		JWTSecret: strings.Repeat("x", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewHandler(service, auth))
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated management request to fail, got %d", response.StatusCode)
	}

	response, err = http.Post(server.URL+"/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"password"}`))
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&login); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || login.AccessToken == "" {
		t.Fatalf("login failed: status=%d token=%q", response.StatusCode, login.AccessToken)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+login.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated management request failed: %d", response.StatusCode)
	}

	response, err = http.Get(server.URL + "/feeds/missing.xml")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("expected public missing feed to return 404, got %d", response.StatusCode)
	}
}
