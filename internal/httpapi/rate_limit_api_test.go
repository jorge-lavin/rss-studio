package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/policy"
)

func TestVersionedAPIRateLimitsAggregateTokensAndReset(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	server, store := newAuthenticatedServerWithClock(t, func() time.Time { return now })
	owner, err := store.User("admin")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	var tokens []string
	for i := 0; i < 3; i++ {
		_, raw, err := store.CreateServiceToken(owner, "preview-agent", []string{domain.ScopeFeedsPreview}, expires)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, raw)
	}

	body := `{"source_url":"https://example.test/rss","outputs":[{"name":"Foo","keyword":"foo"}]}`
	for i := 0; i < policy.PreviewRateLimit; i++ {
		status, _, responseBody := doScopedAPIRequest(t, server.URL, http.MethodPost, "/api/v1/feeds/preview", tokens[i%len(tokens)], body)
		if status != http.StatusOK {
			t.Fatalf("preview %d was limited unexpectedly: status=%d body=%s", i, status, responseBody)
		}
	}

	status, headers, responseBody := doScopedAPIRequest(t, server.URL, http.MethodPost, "/api/v1/feeds/preview", tokens[0], body)
	if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "60" || headers.Get("RateLimit-Limit") != strconv.Itoa(policy.PreviewRateLimit) || headers.Get("RateLimit-Remaining") != "0" || headers.Get("RateLimit-Reset") != "60" {
		t.Fatalf("unexpected rate-limit response: status=%d headers=%v body=%s", status, headers, responseBody)
	}
	var errorResponse struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &errorResponse); err != nil {
		t.Fatal(err)
	}
	if errorResponse.Error.Code != "rate_limited" || strings.Contains(string(responseBody), tokens[0]) {
		t.Fatalf("unsafe or unstable rate-limit response: %s", responseBody)
	}

	if _, err := store.CreateUser("other", "password"); err != nil {
		t.Fatal(err)
	}
	other, err := store.User("other")
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := store.CreateServiceToken(other, "preview-agent", []string{domain.ScopeFeedsPreview}, expires)
	if err != nil {
		t.Fatal(err)
	}
	status, _, responseBody = doScopedAPIRequest(t, server.URL, http.MethodPost, "/api/v1/feeds/preview", otherToken, body)
	if status != http.StatusOK {
		t.Fatalf("other account inherited rate limit: status=%d body=%s", status, responseBody)
	}

	now = now.Add(policy.RateLimitWindow)
	status, headers, responseBody = doScopedAPIRequest(t, server.URL, http.MethodPost, "/api/v1/feeds/preview", tokens[0], body)
	if status != http.StatusOK || headers.Get("RateLimit-Remaining") != strconv.Itoa(policy.PreviewRateLimit-1) {
		t.Fatalf("rate limit did not reset: status=%d headers=%v body=%s", status, headers, responseBody)
	}
}
