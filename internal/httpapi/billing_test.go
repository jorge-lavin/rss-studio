package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/cache"
	"rss-studio/internal/domain"
	"rss-studio/internal/httpapi"
	"rss-studio/internal/rss"
	"rss-studio/internal/storage"
	"rss-studio/internal/stripe"
)

func TestBillingCheckoutRequiresBothFlagsAndReturnsSafeURL(t *testing.T) {
	var calls atomic.Int32
	stripeAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Idempotency-Key") != "checkout-http-fixture" {
			t.Errorf("missing Stripe idempotency key")
		}
		_, _ = writer.Write([]byte(`{"url":"https://checkout.example/session"}`))
	}))
	defer stripeAPI.Close()

	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	admin, err := store.CreateUser("admin", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewServiceWithOperator(store, cache.NewFileCache(t.TempDir()), sourceStub{}, rss.FilterWithOperator)
	stripeService := stripe.New(stripe.Config{
		SecretKey:  "fixture-key",
		PriceID:    "price_fixture",
		SuccessURL: "https://app.example/success",
		CancelURL:  "https://app.example/cancel",
		APIBaseURL: stripeAPI.URL,
		HTTPClient: stripeAPI.Client(),
	}, service.FeatureFlagStore(), service.BillingWebhookStore())
	auth, err := httpapi.NewAuthenticator(httpapi.AuthConfig{UserStore: store})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewHandlerWithStripe(service, time.Now, stripeService, auth))
	defer server.Close()

	token := loginToken(t, server)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/billing/checkout", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"billing_unavailable"`) {
		t.Fatalf("disabled billing returned %d: %s", response.StatusCode, body)
	}

	if _, err := store.SetFeatureFlag(domain.FeatureFlagBillingEnabled, true, admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetFeatureFlag(domain.FeatureFlagBillingStripe, true, admin.ID); err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(http.MethodPost, server.URL+"/api/v1/billing/checkout", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "checkout-http-fixture")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || result["checkout_url"] != "https://checkout.example/session" || len(result) != 1 {
		t.Fatalf("unsafe or unexpected checkout response: %d %s", response.StatusCode, body)
	}
	request, err = http.NewRequest(http.MethodPost, server.URL+"/api/v1/billing/checkout", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "checkout-http-fixture")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != `{"checkout_url":"https://checkout.example/session"}` || calls.Load() != 1 {
		t.Fatalf("checkout retry was not replayed safely: status=%d body=%s calls=%d", response.StatusCode, body, calls.Load())
	}
}

func TestStripeWebhookIsUnauthenticatedAtHTTPBoundary(t *testing.T) {
	server, _ := newAuthenticatedServer(t)
	response, err := http.Post(server.URL+"/api/v1/billing/webhook/stripe", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("unauthenticated webhook reached auth gate or wrong handler: %d %s", response.StatusCode, body)
	}
}
