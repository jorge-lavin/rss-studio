package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
)

var _ application.BillingStore = (*SQLiteStore)(nil)

func TestSQLiteStoreBillingStateReadsAndIdempotentlyUpserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss-studio.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	owner, err := store.CreateUser("alice", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBillingState(owner.ID); !errors.Is(err, ErrBillingStateNotFound) {
		t.Fatalf("expected absent billing state, got %v", err)
	}

	trialStarted := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.FixedZone("test", 3600))
	trialEnds := trialStarted.Add(7 * 24 * time.Hour)
	periodStart := time.Date(2026, time.January, 8, 11, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	input := domain.BillingState{
		AccountID:               owner.ID,
		ProductKey:              domain.BillingProductHostedListenerPersonal,
		Status:                  domain.BillingStatusTrialing,
		TrialStartedAt:          &trialStarted,
		TrialEndsAt:             &trialEnds,
		CurrentPeriodStart:      &periodStart,
		CurrentPeriodEnd:        &periodEnd,
		Provider:                "provider-a",
		ProviderCustomerRef:     "customer-a",
		ProviderSubscriptionRef: "subscription-a",
	}
	first, err := store.UpsertBillingState(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != owner.ID || first.Status != domain.BillingStatusTrialing ||
		first.ProviderCustomerRef != input.ProviderCustomerRef || first.TrialStartedAt == nil ||
		!first.TrialStartedAt.Equal(trialStarted.UTC()) {
		t.Fatalf("unexpected first billing state: %#v", first)
	}

	second, err := store.UpsertBillingState(input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("unchanged upsert was not idempotent: first=%#v second=%#v", first, second)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM billing_states WHERE user_id = ?", owner.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one billing state row, got %d", count)
	}

	updatedInput := input
	updatedInput.Status = domain.BillingStatusActive
	updatedInput.TrialStartedAt = nil
	updatedInput.TrialEndsAt = nil
	updatedInput.ProviderSubscriptionRef = "subscription-b"
	updated, err := store.UpsertBillingState(updatedInput)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != domain.BillingStatusActive || updated.ProviderSubscriptionRef != "subscription-b" ||
		updated.TrialEndsAt != nil || !updated.CreatedAt.Equal(first.CreatedAt) || !updated.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("changed upsert did not replace state atomically: %#v", updated)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetBillingState(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != updated.Status || persisted.ProviderSubscriptionRef != updated.ProviderSubscriptionRef {
		t.Fatalf("billing state did not persist: %#v", persisted)
	}
}

func TestSQLiteStoreBillingStateRejectsInvalidOrUnknownAccounts(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	invalid := domain.BillingState{AccountID: 1, ProductKey: "product", Status: "provider-specific"}
	if _, err := store.UpsertBillingState(invalid); !errors.Is(err, ErrInvalidBillingState) {
		t.Fatalf("expected invalid billing state error, got %v", err)
	}
	unknown := domain.BillingState{
		AccountID:  999,
		ProductKey: domain.BillingProductHostedListenerPersonal,
		Status:     domain.BillingStatusPending,
	}
	if _, err := store.UpsertBillingState(unknown); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected unknown billing account error, got %v", err)
	}
	if _, err := store.GetBillingState(999); !errors.Is(err, ErrBillingStateNotFound) {
		t.Fatalf("expected unknown billing state read to be absent, got %v", err)
	}
}
