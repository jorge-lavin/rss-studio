package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/domain"
)

func serviceTokenForScopes(t *testing.T, store serviceTokenStoreForTest, scopes []string) string {
	t.Helper()
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := store.CreateServiceToken(owner, "test-agent", scopes, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type serviceTokenStoreForTest interface {
	User(string) (domain.User, error)
	CreateServiceToken(domain.User, string, []string, time.Time) (domain.ServiceToken, string, error)
}

func doScopedAPIRequest(t *testing.T, serverURL, method, path, token, body string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, serverURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, data
}

func TestVersionedAPIScopeFailuresAreStableAndIndependent(t *testing.T) {
	server, store := newAuthenticatedServer(t)

	cases := []struct {
		name      string
		method    string
		path      string
		body      string
		required  string
		granted   string
		exactCode int
	}{
		{name: "read", method: http.MethodGet, path: "/api/v1/feeds", required: domain.ScopeFeedsRead, granted: domain.ScopeFeedsPreview, exactCode: http.StatusOK},
		{name: "preview", method: http.MethodPost, path: "/api/v1/feeds/preview", body: `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`, required: domain.ScopeFeedsPreview, granted: domain.ScopeFeedsRead, exactCode: http.StatusOK},
		{name: "write", method: http.MethodPost, path: "/api/v1/feeds", body: `{"name":"Foo","source_url":"https://example.test/rss","keyword":"foo"}`, required: domain.ScopeFeedsWrite, granted: domain.ScopeFeedsRead, exactCode: http.StatusCreated},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/feeds/missing.xml", required: domain.ScopeFeedsDelete, granted: domain.ScopeFeedsRead, exactCode: http.StatusNotFound},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			raw := serviceTokenForScopes(t, store, []string{testCase.granted})
			status, headers, body := doScopedAPIRequest(t, server.URL, testCase.method, testCase.path, raw, testCase.body)
			if status != http.StatusForbidden {
				t.Fatalf("missing %s scope returned %d: %s", testCase.required, status, body)
			}
			if headers.Get("Location") != "" || headers.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("scope failure was not stable JSON: headers=%v body=%s", headers, body)
			}
			if bytes.Contains(body, []byte(raw)) {
				t.Fatal("scope failure leaked bearer token")
			}
			var response struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Code != "insufficient_scope" || response.Error.Message != "Insufficient scope." {
				t.Fatalf("unexpected safe scope error: %#v", response.Error)
			}

			grantedRaw := serviceTokenForScopes(t, store, []string{testCase.required})
			status, _, _ = doScopedAPIRequest(t, server.URL, testCase.method, testCase.path, grantedRaw, testCase.body)
			if status != testCase.exactCode {
				t.Fatalf("granted %s scope returned %d, want %d", testCase.required, status, testCase.exactCode)
			}
		})
	}
}

func TestServiceTokensNeedManagementScope(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	raw := serviceTokenForScopes(t, store, []string{domain.ScopeFeedsRead, domain.ScopeFeedsWrite, domain.ScopeFeedsDelete, domain.ScopeFeedsPreview})
	status, headers, body := doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/auth/tokens", raw, "")
	if status != http.StatusForbidden || headers.Get("Location") != "" {
		t.Fatalf("service token token-management access returned %d: %s", status, body)
	}

	raw = serviceTokenForScopes(t, store, []string{domain.ScopeTokensManage})
	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/auth/tokens", raw, "")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"tokens"`)) {
		t.Fatalf("management-scoped service token was denied: %d: %s", status, body)
	}
}

func TestAdminUsersEndpointRequiresUsersReadAndReturnsSafeSummaries(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	if _, err := store.CreateUser("reader", "password-1234"); err != nil {
		t.Fatal(err)
	}

	feedsToken := serviceTokenForScopes(t, store, []string{domain.ScopeFeedsRead})
	status, _, body := doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/users", feedsToken, "")
	if status != http.StatusForbidden || !strings.Contains(string(body), `"insufficient_scope"`) {
		t.Fatalf("missing users:read scope returned %d: %s", status, body)
	}

	adminToken := serviceTokenForScopes(t, store, []string{domain.ScopeUsersRead})
	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/users", adminToken, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"username":"reader"`) || strings.Contains(string(body), "password_hash") || strings.Contains(string(body), "$2") {
		t.Fatalf("unexpected admin user response: %d: %s", status, body)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/admin/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
	page, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	pageBody, err := io.ReadAll(page.Body)
	page.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "Current accounts") || !strings.Contains(string(pageBody), "reader") {
		t.Fatalf("unexpected admin page: %d: %s", page.StatusCode, pageBody)
	}

	readerToken := loginTokenAs(t, server, "reader", "password-1234")
	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/users", readerToken, "")
	if status != http.StatusForbidden {
		t.Fatalf("non-admin login token accessed users: %d: %s", status, body)
	}
}

func TestPolicyFailureDoesNotExposeTokenInRequestPathOrBody(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	raw := serviceTokenForScopes(t, store, []string{domain.ScopeFeedsRead})
	status, _, body := doScopedAPIRequest(t, server.URL, http.MethodPost, "/api/v1/feeds/preview", raw, `{"source_url":"https://example.test/rss","outputs":[]}`)
	if status != http.StatusForbidden || strings.Contains(string(body), raw) {
		t.Fatalf("unsafe policy failure: status=%d body=%s", status, body)
	}
}
