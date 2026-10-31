package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"rss-studio/internal/domain"
)

type featureFlagAPIResponse struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	UpdatedBy *int64 `json:"updated_by"`
}

func TestAdminFeatureFlagsListReadUpdateAtRuntime(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	adminToken := serviceTokenForScopes(t, store, []string{domain.ScopeUsersRead})

	status, _, body := doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/flags", adminToken, "")
	if status != http.StatusOK {
		t.Fatalf("list flags returned %d: %s", status, body)
	}
	var list struct {
		Flags []featureFlagAPIResponse `json:"flags"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Flags) != len(domain.KnownFeatureFlags()) {
		t.Fatalf("got %d flags, want %d: %s", len(list.Flags), len(domain.KnownFeatureFlags()), body)
	}

	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/flags/"+domain.FeatureFlagBillingEnabled, adminToken, "")
	if status != http.StatusOK {
		t.Fatalf("read flag returned %d: %s", status, body)
	}
	var flag featureFlagAPIResponse
	if err := json.Unmarshal(body, &flag); err != nil {
		t.Fatal(err)
	}
	if flag.Name != domain.FeatureFlagBillingEnabled || flag.Enabled || flag.UpdatedBy != nil {
		t.Fatalf("unexpected initial flag: %#v", flag)
	}

	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodPut, "/api/v1/admin/flags/"+domain.FeatureFlagBillingEnabled, adminToken, `{"enabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("update flag returned %d: %s", status, body)
	}
	if err := json.Unmarshal(body, &flag); err != nil {
		t.Fatal(err)
	}
	if !flag.Enabled || flag.UpdatedBy == nil {
		t.Fatalf("unexpected updated flag: %#v", flag)
	}

	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/flags/"+domain.FeatureFlagBillingEnabled, adminToken, "")
	if status != http.StatusOK {
		t.Fatalf("read after update returned %d: %s", status, body)
	}
	if err := json.Unmarshal(body, &flag); err != nil {
		t.Fatal(err)
	}
	if !flag.Enabled {
		t.Fatalf("runtime read returned stale flag: %#v", flag)
	}
	persisted, err := store.GetFeatureFlag(domain.FeatureFlagBillingEnabled)
	if err != nil || !persisted.Enabled {
		t.Fatalf("flag was not persisted: %#v, %v", persisted, err)
	}
}

func TestAdminFeatureFlagsRequireAdminAndStrictUpdates(t *testing.T) {
	server, store := newAuthenticatedServer(t)
	if _, err := store.CreateUser("reader", "password-1234"); err != nil {
		t.Fatal(err)
	}
	readerToken := loginTokenAs(t, server, "reader", "password-1234")
	status, _, body := doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/flags", readerToken, "")
	if status != http.StatusForbidden || strings.Contains(string(body), readerToken) {
		t.Fatalf("non-admin accessed flags: %d: %s", status, body)
	}

	adminToken := serviceTokenForScopes(t, store, []string{domain.ScopeUsersRead})
	cases := []struct {
		name string
		body string
		code string
	}{
		{name: "missing boolean", body: `{}`, code: "validation_error"},
		{name: "string boolean", body: `{"enabled":"true"}`, code: "validation_error"},
		{name: "unknown field", body: `{"enabled":true,"extra":false}`, code: "unknown_field"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, _, body := doScopedAPIRequest(t, server.URL, http.MethodPut, "/api/v1/admin/flags/"+domain.FeatureFlagBillingStripe, adminToken, testCase.body)
			if status != http.StatusBadRequest || !strings.Contains(string(body), `"code":"`+testCase.code+`"`) {
				t.Fatalf("unexpected strict update response: %d: %s", status, body)
			}
		})
	}

	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodGet, "/api/v1/admin/flags/billing.unknown", adminToken, "")
	if status != http.StatusNotFound || !strings.Contains(string(body), `"code":"not_found"`) {
		t.Fatalf("unknown flag read returned %d: %s", status, body)
	}
}

func TestAdminFeatureFlagBrowserUpdateRequiresCSRF(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	response, err := client.Get(server.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	const marker = `name="csrf_token" value="`
	start := strings.Index(string(page), marker)
	if start < 0 {
		t.Fatal("login page did not contain CSRF token")
	}
	start += len(marker)
	end := strings.IndexByte(string(page)[start:], '"')
	if end < 0 {
		t.Fatal("login page contained malformed CSRF token")
	}
	csrfToken := string(page)[start : start+end]

	response, err = client.PostForm(server.URL+"/auth/login", url.Values{
		"username": {"admin"}, "password": {"password-1234"}, "csrf_token": {csrfToken},
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/admin/flags/"+domain.FeatureFlagBillingEnabled, strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("browser update without CSRF returned %d", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodPut, server.URL+"/api/v1/admin/flags/"+domain.FeatureFlagBillingEnabled, strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrfToken)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("browser update with CSRF returned %d", response.StatusCode)
	}
}
