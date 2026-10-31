package stripe_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/storage"
	"rss-studio/internal/stripe"
)

type enabledFlags struct{}

func (enabledFlags) GetFeatureFlag(name string) (domain.FeatureFlag, error) {
	return domain.FeatureFlag{Name: name, Enabled: true}, nil
}

type disabledFlags struct{}

func (disabledFlags) GetFeatureFlag(name string) (domain.FeatureFlag, error) {
	return domain.FeatureFlag{Name: name}, nil
}

func TestCheckoutUsesFakeStripeAPIAndReturnsOnlyCheckoutURL(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/checkout/sessions" {
			t.Fatalf("unexpected Stripe request: %s %s", request.Method, request.URL.Path)
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "fixture-key" || password != "" {
			t.Fatalf("unexpected Stripe authorization")
		}
		if request.Header.Get("Idempotency-Key") != "checkout-fixture" {
			t.Fatalf("missing Stripe idempotency key")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"mode":                 "subscription",
			"line_items[0][price]": "price_fixture",
			"client_reference_id":  "42",
			"subscription_data[metadata][account_id]":  "42",
			"subscription_data[metadata][product_key]": domain.BillingProductHostedListenerPersonal,
			"subscription_data[trial_period_days]":     "7",
		} {
			if values.Get(key) != want {
				t.Fatalf("Stripe form %s = %q, want %q", key, values.Get(key), want)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"cs_fixture","url":"https://checkout.example/session"}`))
	}))
	defer api.Close()

	service := stripe.New(stripe.Config{
		SecretKey:  "fixture-key",
		PriceID:    "price_fixture",
		SuccessURL: "https://app.example/success",
		CancelURL:  "https://app.example/cancel",
		APIBaseURL: api.URL,
		HTTPClient: api.Client(),
	}, enabledFlags{}, nil)
	checkoutURL, err := service.CheckoutWithIdempotencyKey(t.Context(), 42, "", "checkout-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if checkoutURL != "https://checkout.example/session" {
		t.Fatalf("unexpected checkout URL: %q", checkoutURL)
	}
}

func TestWebhookVerifiesDeduplicatesAndIgnoresOlderEvents(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0).UTC()
	config := stripe.Config{
		WebhookSecret:      "fixture-signing-key",
		Now:                func() time.Time { return now },
		SignatureTolerance: 5 * time.Minute,
	}

	payload := []byte(fmt.Sprintf(`{"id":"evt_fixture_active","type":"customer.subscription.updated","created":1000,"data":{"object":{"id":"sub_fixture","customer":"cus_fixture","status":"active","current_period_start":1100,"current_period_end":1200,"metadata":{"account_id":"%d","product_key":"%s"}}}}`, owner.ID, domain.BillingProductHostedListenerPersonal))
	disabledService := stripe.New(config, disabledFlags{}, store)
	if _, err := disabledService.ProcessWebhook(sign(payload, now.Unix(), "fixture-signing-key"), payload); !errors.Is(err, stripe.ErrBillingDisabled) {
		t.Fatalf("disabled webhook returned %v, want billing disabled", err)
	}
	service := stripe.New(config, enabledFlags{}, store)
	result, err := service.ProcessWebhook(sign(payload, now.Unix(), "fixture-signing-key"), payload)
	if err != nil || !result.Applied || result.Duplicate {
		t.Fatalf("first webhook result=%#v err=%v", result, err)
	}
	result, err = service.ProcessWebhook(sign(payload, now.Unix(), "fixture-signing-key"), payload)
	if err != nil || result.Applied || !result.Duplicate {
		t.Fatalf("duplicate webhook result=%#v err=%v", result, err)
	}

	older := []byte(fmt.Sprintf(`{"id":"evt_fixture_old","type":"customer.subscription.updated","created":900,"data":{"object":{"id":"sub_fixture","customer":"cus_fixture","status":"canceled","metadata":{"account_id":"%d","product_key":"%s"}}}}`, owner.ID, domain.BillingProductHostedListenerPersonal))
	result, err = service.ProcessWebhook(sign(older, now.Unix(), "fixture-signing-key"), older)
	if err != nil || result.Applied || !result.Duplicate {
		t.Fatalf("older webhook result=%#v err=%v", result, err)
	}
	state, err := store.GetBillingState(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != domain.BillingStatusActive || state.ProviderSubscriptionRef != "sub_fixture" {
		t.Fatalf("older webhook changed current state: %#v", state)
	}
	replayed := sign(payload, now.Add(-6*time.Minute).Unix(), "fixture-signing-key")
	if _, err := service.ProcessWebhook(replayed, payload); err == nil {
		t.Fatal("old webhook signature was accepted")
	}
}

func sign(payload []byte, timestamp int64, key string) string {
	digest := hmac.New(sha256.New, []byte(key))
	_, _ = digest.Write([]byte(strconv.FormatInt(timestamp, 10) + "." + string(payload)))
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(digest.Sum(nil))
}
