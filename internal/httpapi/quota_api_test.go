package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/policy"
	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
)

func TestVersionedAPIQuotaResponseIsStableAndValidationDoesNotConsume(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{
		UserStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits := policy.DefaultQuotaLimits()
	limits.PreviewsPerDay = 1
	service := application.NewServiceWithOperatorAndQuotas(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator, limits)
	server := httptest.NewServer(httpapi.NewHandler(service, auth))
	defer server.Close()
	token := loginToken(t, server)
	path := "/api/v1/feeds/preview"
	invalid := `{"source_url":"https://example.test/rss","outputs":[]}`
	status, _, body := doScopedAPIRequest(t, server.URL, http.MethodPost, path, token, invalid)
	if status != http.StatusBadRequest || strings.Contains(string(body), "quota_exceeded") {
		t.Fatalf("invalid request consumed quota: status=%d body=%s", status, body)
	}

	valid := `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`
	status, _, body = doScopedAPIRequest(t, server.URL, http.MethodPost, path, token, valid)
	if status != http.StatusOK {
		t.Fatalf("valid preview failed after validation error: status=%d body=%s", status, body)
	}
	status, headers, body := doScopedAPIRequest(t, server.URL, http.MethodPost, path, token, valid)
	if status != http.StatusTooManyRequests || headers.Get("Location") != "" || headers.Get("Retry-After") == "" {
		t.Fatalf("quota response was not stable: status=%d headers=%v body=%s", status, headers, body)
	}
	var response struct {
		Error struct {
			Code       string `json:"code"`
			Quota      string `json:"quota"`
			Limit      int    `json:"limit"`
			Remaining  int    `json:"remaining"`
			ResetAt    string `json:"reset_at"`
			ResetAfter int    `json:"reset_after"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "quota_exceeded" || response.Error.Quota != string(policy.QuotaPreviewsPerDay) || response.Error.Limit != 1 || response.Error.Remaining != 0 || response.Error.ResetAt == "" || response.Error.ResetAfter <= 0 {
		t.Fatalf("unexpected quota response: %#v", response.Error)
	}
}
