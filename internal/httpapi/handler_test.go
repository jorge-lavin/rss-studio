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
	"rss-studio/internal/domain"
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

func newAuthenticatedServer(t *testing.T) (*httptest.Server, *storage.SQLiteStore) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("admin", "password"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		JWTSecret: strings.Repeat("x", 32),
		UserStore: store,
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	service := application.NewServiceWithOperator(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator)
	server := httptest.NewServer(httpapi.NewHandler(service, auth))
	t.Cleanup(func() {
		server.Close()
		store.Close()
	})
	return server, store
}

func loginToken(t *testing.T, server *httptest.Server) string {
	t.Helper()
	response, err := http.Post(server.URL+"/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"password"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || login.AccessToken == "" {
		t.Fatalf("login failed: status=%d token=%q", response.StatusCode, login.AccessToken)
	}
	return login.AccessToken
}

func assertTailwindUsesClassDarkMode(t *testing.T, page string) {
	t.Helper()
	cdn := strings.Index(page, `<script src="https://cdn.tailwindcss.com"></script>`)
	config := strings.Index(page, `tailwind.config = { darkMode: "class" };`)
	if cdn == -1 || config == -1 || config < cdn {
		t.Fatalf("Tailwind class dark-mode config must run after CDN script: %s", page)
	}
}

func TestHandlerCreatesAndServesFeed(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/feeds", strings.NewReader("name=Foo+feed&source_url=https%3A%2F%2Fexample.test%2Frss&operator=starts_with&keyword=Keep"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected feed creation redirect, got %d", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	homeBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected home page, got %d", response.StatusCode)
	}
	if !strings.Contains(string(homeBody), "RSS Studio · Version "+version.Version) {
		t.Fatalf("home page does not show version %q: %s", version.Version, homeBody)
	}
	if !strings.Contains(string(homeBody), `id="theme-toggle"`) || !strings.Contains(string(homeBody), `darkMode: "class"`) || !strings.Contains(string(homeBody), `rss-studio-theme`) {
		t.Fatalf("home page does not include dark-mode toggle: %s", homeBody)
	}
	if !strings.Contains(string(homeBody), domain.FilterStartsWith.Label()+" “Keep”") {
		t.Fatalf("home page does not show selected filter operator: %s", homeBody)
	}
	assertTailwindUsesClassDarkMode(t, string(homeBody))

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

func TestHomeRendersSplitFeedWorkflow(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected home page, got %d: %s", recorder.Code, recorder.Body.String())
	}

	page := recorder.Body.String()
	for _, marker := range []string{
		`id="split-form"`,
		`action="/feeds/split"`,
		"Split feeds",
		`id="split-source-url"`,
		`data-output-row`,
		`data-output-name`,
		`data-output-operator`,
		`data-output-keyword`,
		`id="add-split-output"`,
		`aria-disabled="false"`,
		`const maxOutputs = 32`,
		`outputs.children.length >= maxOutputs`,
		`Output limit reached`,
		`data-remove-output`,
		`aria-label="Remove output 1"`,
		`id="split-preview"`,
		"Preview split",
		`id="split-publish"`,
		"Publish split feeds",
		`id="split-status"`,
		`role="status"`,
		`aria-live="polite"`,
		`"/preview"`,
		`"/feeds/split"`,
		`credentials: "same-origin"`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("home page missing split workflow marker %q: %s", marker, page)
		}
	}
}

func TestAuthJSONEndpointsRejectOversizedBodies(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	body := `{"username":"admin","password":"password"}` + strings.Repeat(" ", 16<<10)
	for _, endpoint := range []string{"/auth/login", "/auth/register"} {
		request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "password") {
			t.Fatalf("expected safe oversized %s response, got %d: %s", endpoint, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAuthenticatedHandlerProtectsManagementAndLeavesFeedsPublic(t *testing.T) {
	server, _ := newAuthenticatedServer(t)

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

func TestPreviewEndpointReturnsSamplesAndRequiresAuthentication(t *testing.T) {
	server, _ := newAuthenticatedServer(t)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/preview", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"AI","keyword":"foo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated preview to fail, got %d", response.StatusCode)
	}

	token := loginToken(t, server)
	request, err = http.NewRequest(http.MethodPost, server.URL+"/preview", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","operator":"starts_with","keyword":"keep"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected preview success, got %d", response.StatusCode)
	}
	var body struct {
		Outputs []struct {
			Name         string   `json:"name"`
			Operator     string   `json:"operator"`
			Keyword      string   `json:"keyword"`
			Count        int      `json:"count"`
			SampleTitles []string `json:"sample_titles"`
		} `json:"outputs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Outputs) != 1 || body.Outputs[0].Name != "Foo" || body.Outputs[0].Operator != "starts_with" || body.Outputs[0].Keyword != "keep" || body.Outputs[0].Count != 1 || len(body.Outputs[0].SampleTitles) != 1 {
		t.Fatalf("unexpected preview response: %#v", body)
	}
}

func TestPreviewEndpointRejectsMalformedInput(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	request := httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(`{"source_url":`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed preview to return 400, got %d", recorder.Code)
	}
}

func TestSplitJSONEndpointsRejectOversizedBodies(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	body := `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}` + strings.Repeat(" ", 1<<20)
	for _, endpoint := range []string{"/preview", "/feeds/split"} {
		request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("expected oversized %s request to return 400, got %d: %s", endpoint, recorder.Code, recorder.Body.String())
		}
	}
}

func TestPreviewEndpointReturnsBadGatewayForInvalidSource(t *testing.T) {
	service := application.NewService(nil, nil, invalidSourceStub{}, rss.Filter)
	handler := httpapi.NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected invalid source to return 502, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestSplitEndpointRequiresAuthenticationAndReturnsPublicFeeds(t *testing.T) {
	server, _ := newAuthenticatedServer(t)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/feeds/split", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated split request to fail, got %d", response.StatusCode)
	}

	token := loginToken(t, server)
	request, err = http.NewRequest(http.MethodPost, server.URL+"/feeds/split", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Keep","operator":"starts_with","keyword":"keep"},{"name":"Drop","operator":"equals","keyword":"drop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected split success, got %d", response.StatusCode)
	}
	var body struct {
		Outputs []struct {
			Name     string `json:"name"`
			Operator string `json:"operator"`
			Keyword  string `json:"keyword"`
			Path     string `json:"path"`
			URL      string `json:"url"`
		} `json:"outputs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Outputs) != 2 || body.Outputs[0].Name != "Keep" || body.Outputs[0].Operator != "starts_with" || body.Outputs[1].Name != "Drop" || body.Outputs[1].Operator != "equals" {
		t.Fatalf("unexpected split response: %#v", body)
	}
	for _, output := range body.Outputs {
		if output.Path == "" || output.URL != server.URL+output.Path || !strings.HasPrefix(output.Path, "/feeds/") || !strings.HasSuffix(output.Path, ".xml") {
			t.Fatalf("unexpected public feed reference: %#v", output)
		}
	}
}

func TestSplitEndpointRejectsMalformedAndInvalidInput(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	for _, input := range []string{
		`{"source_url":`,
		`{"source_url":"not-a-url","outputs":[{"name":"Foo","keyword":"foo"}]}`,
		`{"source_url":"https://example.test/rss","outputs":[{"name":"","keyword":"foo"}]}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/feeds/split", strings.NewReader(input))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid split request to return 400, got %d: %s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestSplitEndpointReturnsBadGatewayForInvalidSource(t *testing.T) {
	service := application.NewService(nil, nil, invalidSourceStub{}, rss.Filter)
	handler := httpapi.NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/feeds/split", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected invalid split source to return 502, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

type invalidSourceStub struct{}

func (invalidSourceStub) Head(context.Context, string) (time.Time, error) { return time.Time{}, nil }

func (invalidSourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	return []byte("not an RSS feed"), time.Time{}, nil
}

func TestAuthenticatedHandlerIsolatesFeedsByUser(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	service := application.NewServiceWithOperator(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator)
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{JWTSecret: strings.Repeat("x", 32), UserStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Register("reader", "password"); err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := auth.IssueToken("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	readerToken, _, err := auth.IssueToken("reader", "password")
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewHandler(service, auth)

	request := httptest.NewRequest(http.MethodPost, "/feeds", strings.NewReader("name=Alice+feed&source_url=https%3A%2F%2Fexample.test%2Frss&keyword=foo"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+adminToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("admin feed creation failed: %d: %s", recorder.Code, recorder.Body.String())
	}

	for name, token := range map[string]string{"admin": adminToken, "reader": readerToken} {
		request = httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		containsFeed := strings.Contains(recorder.Body.String(), "Alice feed")
		if (name == "admin" && !containsFeed) || (name == "reader" && containsFeed) {
			t.Fatalf("unexpected %s feed visibility: %t", name, containsFeed)
		}
	}

	request = httptest.NewRequest(http.MethodPost, "/feeds/alice-feed/delete", nil)
	request.Header.Set("Authorization", "Bearer "+readerToken)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("reader deleted or found another user's feed: %d", recorder.Code)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/feeds/alice-feed.xml", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("public feed became inaccessible: %d", response.Code)
	}
}

func TestAuthenticatedHandlerSupportsBrowserLogin(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	service := application.NewServiceWithOperator(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator)
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		JWTSecret: strings.Repeat("x", 32),
		UserStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewHandler(service, auth)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "text/html")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected browser request to redirect to login, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	request.Header.Set("Accept", "text/html")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `action="/auth/login"`) || !strings.Contains(recorder.Body.String(), "Login") || !strings.Contains(recorder.Body.String(), `href="/auth/sign-up"`) {
		t.Fatalf("expected login page to be publicly reachable, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `id="theme-toggle"`) || !strings.Contains(recorder.Body.String(), `darkMode: "class"`) || !strings.Contains(recorder.Body.String(), `rss-studio-theme`) {
		t.Fatalf("login page does not include dark-mode toggle: %s", recorder.Body.String())
	}
	assertTailwindUsesClassDarkMode(t, recorder.Body.String())

	request = httptest.NewRequest(http.MethodGet, "/auth/sign-up", nil)
	request.Header.Set("Accept", "text/html")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `action="/auth/sign-up"`) {
		t.Fatalf("expected sign-up page to be publicly reachable, got %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/sign-up", strings.NewReader("username=new-user&password=new-password"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected registration to redirect to login, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader("username=unknown&password=wrong"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Invalid credentials.") {
		t.Fatalf("expected unknown credentials to be rejected, got %d: %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader("username=new-user&password=new-password"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" {
		t.Fatalf("expected registered form login to redirect home, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
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
	if !strings.Contains(recorder.Body.String(), `action="/auth/logout"`) || !strings.Contains(recorder.Body.String(), ">Logout<") {
		t.Fatalf("authenticated home page does not include logout button: %s", recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.AddCookie(cookies[0])
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected logout to redirect to login, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}
	logoutCookies := recorder.Result().Cookies()
	if len(logoutCookies) != 1 || logoutCookies[0].Name != "rss_studio_token" || logoutCookies[0].MaxAge != -1 {
		t.Fatalf("expected auth cookie to be cleared, got %#v", logoutCookies)
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "text/html")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected logged-out home request to redirect to login, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestAuthenticatedHandlerRateLimitsLoginAttempts(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	service := application.NewServiceWithOperator(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator)
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		JWTSecret: strings.Repeat("x", 32),
		UserStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
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
