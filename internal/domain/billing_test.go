package domain

import (
	"testing"
	"time"
)

func TestBillingCatalogContainsProvisionalPersonalHostedListener(t *testing.T) {
	product, ok := BillingProductFor(BillingProductHostedListenerPersonal)
	if !ok {
		t.Fatal("provisional hosted listener product missing")
	}
	if product.Family != BillingProductFamilyHostedListener || product.Plan != BillingPlanPersonal ||
		product.PriceMinor != 500 || product.Currency != BillingCurrencyUSD ||
		product.Interval != BillingIntervalMonth || product.TrialDays != 7 || product.PublishedFeedLimit != 25 {
		t.Fatalf("unexpected provisional product metadata: %#v", product)
	}

	catalog := BillingCatalog()
	if len(catalog) != 1 || catalog[0].Key != product.Key {
		t.Fatalf("unexpected billing catalog: %#v", catalog)
	}
	catalog[0].Key = "changed"
	if current, _ := BillingProductFor(BillingProductHostedListenerPersonal); current.Key != BillingProductHostedListenerPersonal {
		t.Fatal("billing catalog exposed mutable storage")
	}
}

func TestBillingStateValidatesProviderNeutralState(t *testing.T) {
	start := billingTestTime("2026-01-01T00:00:00Z")
	end := billingTestTime("2026-02-01T00:00:00Z")
	state := BillingState{
		AccountID:               1,
		ProductKey:              BillingProductHostedListenerPersonal,
		Status:                  BillingStatusTrialing,
		CurrentPeriodStart:      &start,
		CurrentPeriodEnd:        &end,
		Provider:                "opaque-provider",
		ProviderCustomerRef:     "customer-ref",
		ProviderSubscriptionRef: "subscription-ref",
	}
	if !state.Valid() {
		t.Fatal("valid billing state rejected")
	}
	end = start.Add(-time.Hour)
	if state.Valid() {
		t.Fatal("billing state accepted reversed billing period")
	}
}

func billingTestTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
