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

func testAuth(t *testing.T) *httpapi.Authenticator {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		JWTSecret: strings.Repeat("x", 32),
		UserStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth
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
	if !strings.Contains(string(homeBody), `id="theme-toggle"`) || !strings.Contains(string(homeBody), `darkMode: "class"`) || !strings.Contains(string(homeBody), `rss-studio-theme`) {
		t.Fatalf("home page does not include dark-mode toggle: %s", homeBody)
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
	auth := testAuth(t)
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

func TestAuthenticatedHandlerSupportsBrowserLogin(t *testing.T) {
	store, err := storage.NewJSONStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewService(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.Filter)
	auth := testAuth(t)
	handler := httpapi.NewHandler(service, auth)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "text/html")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/auth/sign-in" {
		t.Fatalf("expected browser request to redirect to sign-in, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodGet, "/auth/sign-in", nil)
	request.Header.Set("Accept", "text/html")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `action="/auth/sign-in"`) || !strings.Contains(recorder.Body.String(), "Sign in") || !strings.Contains(recorder.Body.String(), `href="/auth/sign-up"`) || strings.Contains(recorder.Body.String(), "Self-registration is not available.") {
		t.Fatalf("expected sign-in page to be publicly reachable, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `id="theme-toggle"`) || !strings.Contains(recorder.Body.String(), `darkMode: "class"`) || !strings.Contains(recorder.Body.String(), `rss-studio-theme`) {
		t.Fatalf("sign-in page does not include dark-mode toggle: %s", recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/auth/sign-up", nil)
	request.Header.Set("Accept", "text/html")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `action="/auth/sign-up"`) || !strings.Contains(recorder.Body.String(), "Create account") {
		t.Fatalf("expected sign-up page to be publicly reachable, got %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/sign-up", strings.NewReader("username=new-user&password=new-password"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/auth/sign-in" {
		t.Fatalf("expected registration to redirect to sign-in, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/sign-in", strings.NewReader("username=unknown&password=wrong"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Invalid credentials.") {
		t.Fatalf("expected unknown credentials to be rejected, got %d: %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/sign-in", strings.NewReader("username=new-user&password=new-password"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" {
		t.Fatalf("expected form login to redirect home, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "rss_studio_token" || !cookies[0].HttpOnly {
		t.Fatalf("expected HttpOnly auth cookie, got %#v", cookies)
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookies[0])
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected cookie-authenticated home request to succeed, got %d", recorder.Code)
	}
}

func TestAuthenticatedHandlerRateLimitsLoginAttempts(t *testing.T) {
	store, err := storage.NewJSONStore(filepath.Join(t.TempDir(), "feeds.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewService(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.Filter)
	auth := testAuth(t)
	handler := httpapi.NewHandler(service, auth)

	for attempt := 1; attempt <= 5; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		request.RemoteAddr = "192.0.2.1:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", attempt, recorder.Code)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	request.RemoteAddr = "192.0.2.1:5678"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected rate-limited login to return 429, got %d", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("expected rate-limited login to include Retry-After")
	}
}
