package btcpay_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"rss-studio/internal/btcpay"
	"rss-studio/internal/domain"
	"rss-studio/internal/storage"
)

type enabledFlags struct{}

func (enabledFlags) GetFeatureFlag(name string) (domain.FeatureFlag, error) {
	return domain.FeatureFlag{Name: name, Enabled: true}, nil
}

type disabledFlags struct{}

func (disabledFlags) GetFeatureFlag(name string) (domain.FeatureFlag, error) {
	return domain.FeatureFlag{Name: name}, nil
}

func TestCheckoutUsesPlanCheckoutAPIAndReturnsOnlyURL(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/plan-checkout" {
			t.Fatalf("unexpected BTCPay request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "token fixture-api-key" || request.Header.Get("Idempotency-Key") != "checkout-fixture" {
			t.Fatalf("missing or unsafe BTCPay authorization headers")
		}
		var body map[string]any
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"storeId":    "store_fixture",
			"offeringId": "offering_fixture",
			"planId":     "plan_fixture",
		} {
			if body[key] != want {
				t.Fatalf("BTCPay field %s = %#v, want %q", key, body[key], want)
			}
		}
		metadata, ok := body["newSubscriberMetadata"].(map[string]any)
		if !ok || metadata["account_id"] != "42" || metadata["product_key"] != domain.BillingProductHostedListenerPersonal {
			t.Fatalf("missing account metadata: %#v", body["newSubscriberMetadata"])
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"plancheckout_fixture","url":"https://pay.example/plan-checkout/fixture"}`))
	}))
	defer api.Close()

	service := btcpay.New(btcpay.Config{
		BaseURL:    api.URL,
		APIKey:     "fixture-api-key",
		StoreID:    "store_fixture",
		OfferingID: "offering_fixture",
		PlanID:     "plan_fixture",
		SuccessURL: "https://app.example/success",
		HTTPClient: api.Client(),
	}, enabledFlags{}, nil)
	checkoutURL, err := service.CheckoutWithIdempotencyKey(t.Context(), 42, "", "checkout-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if checkoutURL != "https://pay.example/plan-checkout/fixture" {
		t.Fatalf("unexpected checkout URL: %q", checkoutURL)
	}
}

func TestWebhookVerifiesFlagsDeduplicatesAndOrdersEvents(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	config := btcpay.Config{
		WebhookSecret: "fixture-signing-key",
		StoreID:       "store_fixture",
		OfferingID:    "offering_fixture",
		PlanID:        "plan_fixture",
	}
	payload := []byte(fmt.Sprintf(`{"deliveryId":"delivery_active","type":"SubscriberPhaseChanged","timestamp":1000,"storeId":"store_fixture","subscriber":{"created":900,"customer":{"id":"cust_fixture"},"offering":{"id":"offering_fixture"},"plan":{"id":"plan_fixture"},"periodEnd":2000,"isActive":true,"phase":"Normal","metadata":{"account_id":"%d","product_key":"%s"}}}`, owner.ID, domain.BillingProductHostedListenerPersonal))

	disabledService := btcpay.New(config, disabledFlags{}, store)
	if _, err := disabledService.ProcessWebhook(sign(payload, "fixture-signing-key"), payload); !errors.Is(err, btcpay.ErrBillingDisabled) {
		t.Fatalf("disabled webhook returned %v, want billing disabled", err)
	}

	service := btcpay.New(config, enabledFlags{}, store)
	result, err := service.ProcessWebhook(sign(payload, "fixture-signing-key"), payload)
	if err != nil || !result.Applied || result.Duplicate {
		t.Fatalf("first webhook result=%#v err=%v", result, err)
	}
	result, err = service.ProcessWebhook(sign(payload, "fixture-signing-key"), payload)
	if err != nil || result.Applied || !result.Duplicate {
		t.Fatalf("duplicate webhook result=%#v err=%v", result, err)
	}

	older := []byte(fmt.Sprintf(`{"deliveryId":"delivery_old","type":"SubscriberDisabled","timestamp":900,"storeId":"store_fixture","reason":"Expired","subscriber":{"customer":{"id":"cust_fixture"},"offering":{"id":"offering_fixture"},"plan":{"id":"plan_fixture"},"isActive":false,"phase":"Expired","metadata":{"account_id":"%d","product_key":"%s"}}}`, owner.ID, domain.BillingProductHostedListenerPersonal))
	result, err = service.ProcessWebhook(sign(older, "fixture-signing-key"), older)
	if err != nil || result.Applied || !result.Duplicate {
		t.Fatalf("older webhook result=%#v err=%v", result, err)
	}
	state, err := store.GetBillingState(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != domain.BillingStatusActive || state.ProviderCustomerRef != "cust_fixture" {
		t.Fatalf("older webhook changed current state: %#v", state)
	}
}

func sign(payload []byte, key string) string {
	digest := hmac.New(sha256.New, []byte(key))
	_, _ = digest.Write(payload)
	return "sha256=" + hex.EncodeToString(digest.Sum(nil))
}
