package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/domain"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/observability"
	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
	"rss-studio/internal/version"
)

type sourceStub struct{}

func (sourceStub) Head(context.Context, string) (time.Time, error) { return time.Time{}, nil }

func (sourceStub) Get(context.Context, string) ([]byte, time.Time, error) {
	return []byte(`<rss version="2.0"><channel><title>Source</title><item><title>Keep foo</title><guid>1</guid></item><item><title>Drop</title><guid>2</guid></item></channel></rss>`), time.Time{}, nil
}

type failingMutationCache struct {
	data        map[string][]byte
	writes      int
	failOnWrite int
}

type requestAuditRecorder struct {
	events []observability.Event
}

func (r *requestAuditRecorder) Record(event observability.Event) {
	r.events = append(r.events, event)
}

func (c *failingMutationCache) Read(feed domain.Feed) ([]byte, error) {
	data, ok := c.data[feed.Path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (c *failingMutationCache) ReadMetadata(domain.Feed) (application.CacheMetadata, error) {
	return application.CacheMetadata{}, fs.ErrNotExist
}

func (c *failingMutationCache) Write(feed domain.Feed, data []byte, _ time.Time) error {
	c.writes++
	if c.writes == c.failOnWrite {
		return errors.New("cache write failed")
	}
	c.data[feed.Path] = append([]byte(nil), data...)
	return nil
}

func (c *failingMutationCache) Remove(feed domain.Feed) error {
	delete(c.data, feed.Path)
	return nil
}

func newAuthenticatedServer(t *testing.T) (*httptest.Server, *storage.SQLiteStore) {
	return newAuthenticatedServerWithSource(t, sourceStub{})
}

func newAuthenticatedServerWithSource(t *testing.T, feedSource application.Source) (*httptest.Server, *storage.SQLiteStore) {
	return newAuthenticatedServerWithSourceAndClock(t, feedSource, time.Now)
}

func newAuthenticatedServerWithClock(t *testing.T, now func() time.Time) (*httptest.Server, *storage.SQLiteStore) {
	return newAuthenticatedServerWithSourceAndClock(t, sourceStub{}, now)
}

func newAuthenticatedServerWithSourceAndClock(t *testing.T, feedSource application.Source, now func() time.Time) (*httptest.Server, *storage.SQLiteStore) {
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
	service := application.NewServiceWithOperator(store, cache.NewFileCache(t.TempDir()), feedSource, rss.FilterWithOperator)
	server := httptest.NewServer(httpapi.NewHandlerWithClock(service, now, auth))
	t.Cleanup(func() {
		server.Close()
		store.Close()
	})
	return server, store
}

func loginToken(t *testing.T, server *httptest.Server) string {
	return loginTokenAs(t, server, "admin", "password")
}

func loginTokenAs(t *testing.T, server *httptest.Server, username, password string) string {
	t.Helper()
	response, err := http.Post(server.URL+"/auth/login", "application/json", strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
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

func TestAPIDocumentationPageIsPublicAndUsesAppShell(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	for _, path := range []string{"/api", "/api/"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		page := string(body)
		if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("expected public HTML API page for %s, got %d %q: %s", path, response.StatusCode, response.Header.Get("Content-Type"), page)
		}
		for _, marker := range []string{
			"Developer API · v1",
			"/auth/login",
			"/feeds/preview",
			"Authorization: Bearer",
			`id="theme-toggle"`,
		} {
			if !strings.Contains(page, marker) {
				t.Fatalf("API page %s missing marker %q: %s", path, marker, page)
			}
		}
		assertTailwindUsesClassDarkMode(t, page)
	}
}

func TestRateRejectionAuditEventUsesAccountIdentity(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	server, _ := newAuthenticatedServerWithClock(t, func() time.Time { return now })
	token := loginToken(t, server)
	recorder := &requestAuditRecorder{}
	body := `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`

	for attempt := 0; attempt < 21; attempt++ {
		request := httptest.NewRequest(http.MethodPost, server.URL+"/api/v1/feeds/preview", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request = request.WithContext(observability.WithRecorder(request.Context(), recorder))
		response := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(response, request)
		if attempt < 20 && response.Code != http.StatusOK {
			t.Fatalf("preview attempt %d failed: %d %s", attempt, response.Code, response.Body.String())
		}
		if attempt == 20 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("expected rate rejection, got %d %s", response.Code, response.Body.String())
		}
	}

	found := false
	for _, event := range recorder.events {
		if !event.RateRejected {
			continue
		}
		found = true
		if event.Operation != "feeds.preview" || event.ActorID == 0 || event.TokenID != 0 || event.Result != "rejected" {
			t.Fatalf("unexpected rate rejection audit event: %#v", event)
		}
	}
	if !found {
		t.Fatal("missing rate rejection audit event")
	}
}

func TestServiceTokenManagementReturnsRawTokenOnceAndSupportsRevocation(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	ownerToken := loginToken(t, server)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/auth/tokens", strings.NewReader(`{"name":"agent","scopes":["feeds:write","feeds:read"],"expires_in":3600}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+ownerToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID     int64    `json:"id"`
		Token  string   `json:"token"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || created.ID == 0 || created.Token == "" {
		t.Fatalf("service token creation failed: status=%d response=%#v", response.StatusCode, created)
	}
	if len(created.Scopes) != 2 || created.Scopes[0] != domain.ScopeFeedsRead || created.Scopes[1] != domain.ScopeFeedsWrite {
		t.Fatalf("scopes were not canonicalized: %#v", created.Scopes)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+ownerToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || strings.Contains(string(body), created.Token) {
		t.Fatalf("service token list leaked raw token: status=%d body=%s", response.StatusCode, body)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/v1/feeds", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+created.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("service token authentication failed: %d", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodPost, server.URL+"/api/v1/auth/tokens/"+strconv.FormatInt(created.ID, 10)+"/revoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+ownerToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("service token revocation failed: %d", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/v1/feeds", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+created.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked service token accepted: %d", response.StatusCode)
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

		request = httptest.NewRequest(http.MethodGet, "/api/v1/feeds", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var apiFeeds struct {
			Feeds []json.RawMessage `json:"feeds"`
		}
		if recorder.Code != http.StatusOK || json.NewDecoder(recorder.Body).Decode(&apiFeeds) != nil {
			t.Fatalf("unexpected %s API feed list: %d %s", name, recorder.Code, recorder.Body.String())
		}
		if (name == "admin" && len(apiFeeds.Feeds) != 1) || (name == "reader" && len(apiFeeds.Feeds) != 0) {
			t.Fatalf("unexpected %s API feed visibility: %d", name, len(apiFeeds.Feeds))
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

func TestVersionedAuthEndpointsReturnJSONAndPropagateRequestIDs(t *testing.T) {
	server, _ := newAuthenticatedServer(t)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"username":"api-user","password":"api-password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "client-request-1")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || recorder.Header().Get("Location") != "" {
		t.Fatalf("expected JSON registration without redirect, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if recorder.Header().Get("X-Request-ID") != "client-request-1" {
		t.Fatalf("expected request ID propagation, got %q", recorder.Header().Get("X-Request-ID"))
	}
	var registration struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&registration); err != nil {
		t.Fatal(err)
	}
	if registration.Username != "api-user" {
		t.Fatalf("unexpected registration response: %#v", registration)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"api-user","password":"api-password"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Location") != "" {
		t.Fatalf("expected JSON login without redirect, got %d and %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if recorder.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected generated request ID")
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	if login.AccessToken == "" {
		t.Fatal("expected access token")
	}
}

func TestVersionedAuthEndpointsUseStableJSONErrors(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	tests := []struct {
		name        string
		path        string
		body        string
		status      int
		code        string
		contentType string
	}{
		{name: "malformed JSON", path: "/api/v1/auth/login", body: `{"username":`, status: http.StatusBadRequest, code: "invalid_json"},
		{name: "unknown field", path: "/api/v1/auth/login", body: `{"username":"admin","password":"password","extra":true}`, status: http.StatusBadRequest, code: "unknown_field"},
		{name: "invalid credentials", path: "/api/v1/auth/login", body: `{"username":"admin","password":"wrong"}`, status: http.StatusUnauthorized, code: "invalid_credentials"},
		{name: "validation", path: "/api/v1/auth/register", body: `{"username":"","password":""}`, status: http.StatusBadRequest, code: "validation_error"},
		{name: "unsupported content type", path: "/api/v1/auth/register", body: "username=new-user&password=new-password", status: http.StatusBadRequest, code: "unsupported_media_type", contentType: "application/x-www-form-urlencoded"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.RemoteAddr = "192.0.2." + strconv.Itoa(index+10) + ":1234"
			contentType := test.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Accept", "text/html")
			recorder := httptest.NewRecorder()
			server.Config.Handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status || recorder.Header().Get("Location") != "" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("unexpected API response: status=%d location=%q content-type=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			var response struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				RequestID string `json:"request_id"`
			}
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Code != test.code || response.Error.Message == "" || response.RequestID == "" || recorder.Header().Get("X-Request-ID") != response.RequestID {
				t.Fatalf("unexpected API error: %#v, header request ID=%q", response, recorder.Header().Get("X-Request-ID"))
			}
		})
	}
}

func TestVersionedAPIAuthenticationNeverRedirects(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/v1/feeds"},
		{method: http.MethodPost, path: "/api/v1/feeds", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/feeds/preview", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/feeds/split", body: `{}`},
		{method: http.MethodDelete, path: "/api/v1/feeds/missing"},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Accept", "text/html")
		recorder := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("Location") != "" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("expected JSON API authentication error for %s %s, got %d, location=%q, content-type=%q, body=%s", test.method, test.path, recorder.Code, recorder.Header().Get("Location"), recorder.Header().Get("Content-Type"), recorder.Body.String())
		}
	}
}

func TestVersionedCreateReturnsFeedMetadata(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds", strings.NewReader(`{"name":"Foo feed","source_url":"https://example.test/rss","operator":"starts_with","keyword":"Keep"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || recorder.Header().Get("Location") != "" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("expected feed metadata JSON without redirect, got %d and %q: %s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	var feed struct {
		Name      string    `json:"name"`
		Path      string    `json:"path"`
		URL       string    `json:"url"`
		SourceURL string    `json:"source_url"`
		Operator  string    `json:"operator"`
		Keyword   string    `json:"keyword"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&feed); err != nil {
		t.Fatal(err)
	}
	if feed.Name != "Foo feed" || feed.Path != "/feeds/foo-feed.xml" || feed.URL != "http://example.com/feeds/foo-feed.xml" || feed.SourceURL != "https://example.test/rss" || feed.Operator != "starts_with" || feed.Keyword != "Keep" || feed.CreatedAt.IsZero() {
		t.Fatalf("unexpected created feed metadata: %#v", feed)
	}
}

func TestVersionedCreateIdempotencyReplaysAndRejectsChangedPayload(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	token := loginToken(t, server)
	body := `{"name":"Idempotent feed","source_url":"https://example.test/rss","keyword":"foo"}`
	status, first := doIdempotentRequest(t, server, token, "/api/v1/feeds", "create-key", body)
	if status != http.StatusCreated {
		t.Fatalf("first create returned %d: %s", status, first)
	}
	status, retry := doIdempotentRequest(t, server, token, "/api/v1/feeds", "create-key", body)
	if status != http.StatusCreated || !bytes.Equal(first, retry) {
		t.Fatalf("retry did not replay original result: status=%d first=%s retry=%s", status, first, retry)
	}
	status, conflict := doIdempotentRequest(t, server, token, "/api/v1/feeds", "create-key", `{"name":"Changed feed","source_url":"https://example.test/rss","keyword":"foo"}`)
	if status != http.StatusConflict || !bytes.Contains(conflict, []byte(`"idempotency_conflict"`)) || bytes.Contains(conflict, []byte("create-key")) {
		t.Fatalf("changed create was not safely rejected: status=%d body=%s", status, conflict)
	}
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 {
		t.Fatalf("idempotent create created %d feeds", len(feeds))
	}
}

func TestVersionedSplitIdempotencyReplaysOneDurableResult(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	token := loginToken(t, server)
	body := `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"},{"name":"Keep","keyword":"keep"}]}`
	status, first := doIdempotentRequest(t, server, token, "/api/v1/feeds/split", "split-key", body)
	if status != http.StatusCreated {
		t.Fatalf("first split returned %d: %s", status, first)
	}
	status, retry := doIdempotentRequest(t, server, token, "/api/v1/feeds/split", "split-key", body)
	if status != http.StatusCreated || !bytes.Equal(first, retry) {
		t.Fatalf("split retry did not replay original result: status=%d first=%s retry=%s", status, first, retry)
	}
	status, conflict := doIdempotentRequest(t, server, token, "/api/v1/feeds/split", "split-key", `{"source_url":"https://example.test/rss","outputs":[{"name":"Changed","keyword":"foo"},{"name":"Keep","keyword":"keep"}]}`)
	if status != http.StatusConflict || !bytes.Contains(conflict, []byte(`"idempotency_conflict"`)) {
		t.Fatalf("changed split was not rejected: status=%d body=%s", status, conflict)
	}
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 2 {
		t.Fatalf("idempotent split created %d feeds", len(feeds))
	}
}

func TestVersionedIdempotencyBindsOwnerAndCredential(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	adminToken := loginToken(t, server)
	if response, err := http.Post(server.URL+"/api/v1/auth/register", "application/json", strings.NewReader(`{"username":"reader","password":"reader-password"}`)); err != nil {
		t.Fatal(err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("reader registration failed: %d", response.StatusCode)
		}
	}
	readerToken := loginTokenAs(t, server, "reader", "reader-password")
	body := `{"name":"Owner feed","source_url":"https://example.test/rss","keyword":"foo"}`
	if status, _ := doIdempotentRequest(t, server, adminToken, "/api/v1/feeds", "owner-key", body); status != http.StatusCreated {
		t.Fatalf("admin create returned %d", status)
	}
	if status, _ := doIdempotentRequest(t, server, readerToken, "/api/v1/feeds", "owner-key", body); status != http.StatusCreated {
		t.Fatalf("reader create returned %d", status)
	}
	unauthenticated, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/feeds", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated.Header.Set("Content-Type", "application/json")
	unauthenticated.Header.Set("Idempotency-Key", "owner-key")
	response, err := http.DefaultClient.Do(unauthenticated)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || bytes.Contains(data, []byte("owner-key")) {
		t.Fatalf("unauthenticated key claim was not rejected safely: status=%d body=%s", response.StatusCode, data)
	}

	admin, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	_, serviceToken, err := store.CreateServiceToken(admin, "write-agent", []string{domain.ScopeFeedsWrite}, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := doIdempotentRequest(t, server, serviceToken, "/api/v1/feeds", "owner-key", body); status != http.StatusCreated {
		t.Fatalf("service-token create returned %d", status)
	}
	reader, err := store.User("reader")
	if err != nil {
		t.Fatal(err)
	}
	adminFeeds, err := store.List(admin)
	if err != nil {
		t.Fatal(err)
	}
	readerFeeds, err := store.List(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(adminFeeds) != 2 || len(readerFeeds) != 1 {
		t.Fatalf("idempotency key crossed tenant boundary: admin=%d reader=%d", len(adminFeeds), len(readerFeeds))
	}
}

func TestVersionedCreateIdempotencyConcurrentRequestsCreateOneFeed(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	token := loginToken(t, server)
	body := `{"name":"Concurrent feed","source_url":"https://example.test/rss","keyword":"foo"}`
	const requestCount = 8
	start := make(chan struct{})
	statuses := make(chan int, requestCount)
	var wait sync.WaitGroup
	for i := 0; i < requestCount; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			status, _ := doIdempotentRequest(t, server, token, "/api/v1/feeds", "concurrent-key", body)
			statuses <- status
		}()
	}
	close(start)
	wait.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("concurrent retry returned %d", status)
		}
	}
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 {
		t.Fatalf("concurrent idempotent create created %d feeds", len(feeds))
	}
}

func doIdempotentRequest(t *testing.T, server *httptest.Server, token, path, key, body string) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

func TestVersionedSplitReturnsAllFeedReferences(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/split", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Keep","operator":"starts_with","keyword":"keep"},{"name":"Drop","operator":"equals","keyword":"drop"}]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || recorder.Header().Get("Location") != "" {
		t.Fatalf("expected split creation without redirect, got %d and %q: %s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	var response struct {
		Outputs []struct {
			Name     string `json:"name"`
			Operator string `json:"operator"`
			Keyword  string `json:"keyword"`
			Path     string `json:"path"`
			URL      string `json:"url"`
		} `json:"outputs"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Outputs) != 2 || response.Outputs[0].Name != "Keep" || response.Outputs[0].Operator != "starts_with" || response.Outputs[1].Name != "Drop" || response.Outputs[1].Operator != "equals" {
		t.Fatalf("unexpected split response: %#v", response)
	}
	for _, output := range response.Outputs {
		if output.Path == "" || output.URL != "http://example.com"+output.Path || !strings.HasPrefix(output.Path, "/feeds/") || !strings.HasSuffix(output.Path, ".xml") {
			t.Fatalf("unexpected split feed reference: %#v", output)
		}
	}
}

func TestVersionedSplitReturnsErrorAfterRollback(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.CreateUser("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{JWTSecret: strings.Repeat("x", 32), UserStore: store})
	if err != nil {
		t.Fatal(err)
	}
	cache := &failingMutationCache{data: make(map[string][]byte), failOnWrite: 2}
	service := application.NewServiceWithOperator(store, cache, sourceStub{}, rss.FilterWithOperator)
	token, _, err := auth.IssueToken("admin", "password")
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/split", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"},{"name":"Keep","keyword":"keep"}]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	httpapi.NewHandler(service, auth).ServeHTTP(recorder, request)
	assertAPIError(t, recorder, http.StatusInternalServerError, "internal_error")

	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 0 || len(cache.data) != 0 {
		t.Fatalf("split rollback left state: feeds=%#v cache=%#v", feeds, cache.data)
	}
}

func TestVersionedCreateIdempotencyReleasesKeyAfterFailure(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{JWTSecret: strings.Repeat("x", 32), UserStore: store})
	if err != nil {
		t.Fatal(err)
	}
	cache := &failingMutationCache{data: make(map[string][]byte), failOnWrite: 1}
	service := application.NewServiceWithOperator(store, cache, sourceStub{}, rss.FilterWithOperator)
	token, _, err := auth.IssueToken("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewHandler(service, auth)
	body := `{"name":"Retryable feed","source_url":"https://example.test/rss","keyword":"foo"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "failure-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assertAPIError(t, recorder, http.StatusBadGateway, "source_unavailable")

	cache.failOnWrite = 0
	request = httptest.NewRequest(http.MethodPost, "/api/v1/feeds", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "failure-key")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("retry after failed mutation returned %d: %s", recorder.Code, recorder.Body.String())
	}
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	feeds, err := store.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 {
		t.Fatalf("failed idempotent mutation left unexpected feeds: %#v", feeds)
	}
}

func TestVersionedMutationsReturnStableJSONErrors(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		code   string
	}{
		{name: "create malformed JSON", method: http.MethodPost, path: "/api/v1/feeds", body: `{"name":`, code: "invalid_json"},
		{name: "create unknown field", method: http.MethodPost, path: "/api/v1/feeds", body: `{"name":"Foo","source_url":"https://example.test/rss","keyword":"foo","extra":true}`, code: "unknown_field"},
		{name: "create invalid source", method: http.MethodPost, path: "/api/v1/feeds", body: `{"name":"Foo","source_url":"file:///tmp/feed.xml","keyword":"foo"}`, code: "validation_error"},
		{name: "split invalid operator", method: http.MethodPost, path: "/api/v1/feeds/split", body: `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","operator":"bogus","keyword":"foo"}]}`, code: "validation_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "text/html")
			recorder := httptest.NewRecorder()
			server.Config.Handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Location") != "" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("unexpected mutation error: status=%d location=%q content-type=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			var response apiErrorTestResponse
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Code != test.code || response.Error.Message == "" || response.RequestID == "" {
				t.Fatalf("unexpected mutation error response: %#v", response)
			}
		})
	}
}

func TestVersionedDeleteIsTenantScopedAndRepeatable(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	adminToken := loginToken(t, server)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds", strings.NewReader(`{"name":"Foo feed","source_url":"https://example.test/rss","keyword":"foo"}`))
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected feed creation, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	feedPath := strings.TrimSuffix(strings.TrimPrefix(created.Path, "/feeds/"), ".xml")

	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"username":"reader","password":"reader-password"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected reader registration, got %d: %s", recorder.Code, recorder.Body.String())
	}
	readerToken := loginTokenAs(t, server, "reader", "reader-password")

	request = httptest.NewRequest(http.MethodDelete, "/api/v1/feeds/"+feedPath, nil)
	request.Header.Set("Authorization", "Bearer "+readerToken)
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	assertAPIError(t, recorder, http.StatusNotFound, "not_found")

	request = httptest.NewRequest(http.MethodDelete, "/api/v1/feeds/"+feedPath, nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Location") != "" {
		t.Fatalf("expected delete success without redirect, got %d and %q: %s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	var deleted struct {
		Deleted bool   `json:"deleted"`
		Path    string `json:"path"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&deleted); err != nil {
		t.Fatal(err)
	}
	if !deleted.Deleted || deleted.Path != feedPath {
		t.Fatalf("unexpected delete response: %#v", deleted)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/v1/feeds/"+feedPath+".xml", nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	assertAPIError(t, recorder, http.StatusNotFound, "not_found")

	response, err := http.Get(server.URL + "/feeds/" + feedPath + ".xml")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("expected deleted public feed to return 404, got %d", response.StatusCode)
	}
}

func TestVersionedFeedListReturnsTenantScopedMetadata(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)

	request := httptest.NewRequest(http.MethodPost, "/feeds", strings.NewReader("name=Foo+feed&source_url=https%3A%2F%2Fexample.test%2Frss&operator=starts_with&keyword=Keep"))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("expected feed creation redirect, got %d: %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/feeds", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("expected feed list JSON, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Feeds []struct {
			Name      string    `json:"name"`
			Path      string    `json:"path"`
			URL       string    `json:"url"`
			SourceURL string    `json:"source_url"`
			Operator  string    `json:"operator"`
			Keyword   string    `json:"keyword"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"feeds"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Feeds) != 1 {
		t.Fatalf("unexpected feed list: %#v", response)
	}
	feed := response.Feeds[0]
	if feed.Name != "Foo feed" || feed.Path != "/feeds/foo-feed.xml" || feed.URL != "http://example.com/feeds/foo-feed.xml" || feed.SourceURL != "https://example.test/rss" || feed.Operator != "starts_with" || feed.Keyword != "Keep" || feed.CreatedAt.IsZero() {
		t.Fatalf("unexpected feed metadata: %#v", feed)
	}
}

func TestVersionedPreviewReturnsResultsWithoutPersisting(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/preview", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","operator":"starts_with","keyword":"keep"}]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected preview success, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		Outputs []struct {
			Name         string   `json:"name"`
			Operator     string   `json:"operator"`
			Keyword      string   `json:"keyword"`
			Count        int      `json:"count"`
			SampleTitles []string `json:"sample_titles"`
		} `json:"outputs"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Outputs) != 1 || preview.Outputs[0].Name != "Foo" || preview.Outputs[0].Operator != "starts_with" || preview.Outputs[0].Keyword != "keep" || preview.Outputs[0].Count != 1 || len(preview.Outputs[0].SampleTitles) != 1 {
		t.Fatalf("unexpected preview response: %#v", preview)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/feeds", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	var feeds struct {
		Feeds []json.RawMessage `json:"feeds"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&feeds); err != nil {
		t.Fatal(err)
	}
	if len(feeds.Feeds) != 0 {
		t.Fatalf("preview persisted feeds: %#v", feeds)
	}
}

func TestVersionedPreviewRejectsMalformedAndInvalidInput(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	tests := []struct {
		name        string
		body        string
		contentType string
		code        string
	}{
		{name: "malformed JSON", body: `{"source_url":`, contentType: "application/json", code: "invalid_json"},
		{name: "unknown field", body: `{"source_url":"https://example.test/rss","outputs":[],"extra":true}`, contentType: "application/json", code: "unknown_field"},
		{name: "invalid operator", body: `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","operator":"bogus","keyword":"foo"}]}`, contentType: "application/json", code: "validation_error"},
		{name: "unsupported content type", body: `{}`, contentType: "text/plain", code: "unsupported_media_type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/preview", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", test.contentType)
			recorder := httptest.NewRecorder()
			server.Config.Handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Location") != "" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("unexpected preview error: status=%d location=%q content-type=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			var response apiErrorTestResponse
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Code != test.code || response.Error.Message == "" || response.RequestID == "" {
				t.Fatalf("unexpected API error: %#v", response)
			}
		})
	}
}

func TestVersionedPreviewRejectsInvalidSource(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/preview", strings.NewReader(`{"source_url":"file:///tmp/feed.xml","outputs":[{"name":"Foo","keyword":"foo"}]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid source URL to return 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response apiErrorTestResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "validation_error" {
		t.Fatalf("unexpected invalid source error: %#v", response)
	}
}

func TestVersionedPreviewReturnsBadGatewayForInvalidSourceFeed(t *testing.T) {
	server, _ := newAuthenticatedServerWithSource(t, invalidSourceStub{})
	token := loginToken(t, server)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/preview", strings.NewReader(`{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected invalid source feed to return 502, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response apiErrorTestResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "source_unavailable" {
		t.Fatalf("unexpected invalid source error: %#v", response)
	}
}

func TestVersionedPreviewRejectsOversizedBody(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	token := loginToken(t, server)
	body := `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}` + strings.Repeat(" ", 1<<20)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/preview", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("expected oversized API preview to return JSON 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

type apiErrorTestResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func assertAPIError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status || recorder.Header().Get("Location") != "" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected API error: status=%d location=%q content-type=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	var response apiErrorTestResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != code || response.Error.Message == "" || response.RequestID == "" {
		t.Fatalf("unexpected API error response: %#v", response)
	}
}
