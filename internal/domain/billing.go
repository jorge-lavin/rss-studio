package domain

import (
	"errors"
	"time"
	"unicode/utf8"
)

const (
	BillingProductHostedListenerPersonal = "hosted_listener_personal"
	BillingProductFamilyHostedListener   = "hosted_listener"
	BillingPlanPersonal                  = "personal"
	BillingCurrencyUSD                   = "USD"
	BillingIntervalMonth                 = "month"
)

var (
	ErrBillingStateNotFound = errors.New("billing state not found")
	ErrInvalidBillingState  = errors.New("invalid billing state")
)

// BillingProduct is provider-neutral catalog metadata. It is not an
// entitlement decision and its feed limit is intentionally not enforced here.
type BillingProduct struct {
	Key                string
	Name               string
	Family             string
	Plan               string
	PriceMinor         int64
	Currency           string
	Interval           string
	TrialDays          int
	PublishedFeedLimit int
}

var billingProducts = [...]BillingProduct{
	{
		Key:                BillingProductHostedListenerPersonal,
		Name:               "Hosted Listener",
		Family:             BillingProductFamilyHostedListener,
		Plan:               BillingPlanPersonal,
		PriceMinor:         500,
		Currency:           BillingCurrencyUSD,
		Interval:           BillingIntervalMonth,
		TrialDays:          7,
		PublishedFeedLimit: 25,
	},
}

// BillingCatalog returns a copy of provisional product metadata.
func BillingCatalog() []BillingProduct {
	return append([]BillingProduct(nil), billingProducts[:]...)
}

func BillingProductFor(key string) (BillingProduct, bool) {
	for _, product := range billingProducts {
		if product.Key == key {
			return product, true
		}
	}
	return BillingProduct{}, false
}

type BillingStatus string

const (
	BillingStatusPending  BillingStatus = "pending"
	BillingStatusTrialing BillingStatus = "trialing"
	BillingStatusActive   BillingStatus = "active"
	BillingStatusPastDue  BillingStatus = "past_due"
	BillingStatusCanceled BillingStatus = "canceled"
	BillingStatusExpired  BillingStatus = "expired"
)

// BillingState is the provider-neutral current billing state for one account.
// Provider fields contain opaque identifiers only; they are not credentials.
type BillingState struct {
	AccountID               int64
	ProductKey              string
	Status                  BillingStatus
	TrialStartedAt          *time.Time
	TrialEndsAt             *time.Time
	CurrentPeriodStart      *time.Time
	CurrentPeriodEnd        *time.Time
	Provider                string
	ProviderCustomerRef     string
	ProviderSubscriptionRef string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

func (state BillingState) Valid() bool {
	if state.AccountID <= 0 || state.ProductKey == "" || !validBillingStatus(state.Status) {
		return false
	}
	if !validBillingText(state.Provider, 100) ||
		!validBillingText(state.ProviderCustomerRef, 255) ||
		!validBillingText(state.ProviderSubscriptionRef, 255) {
		return false
	}
	return state.CurrentPeriodStart == nil || state.CurrentPeriodEnd == nil ||
		!state.CurrentPeriodEnd.Before(*state.CurrentPeriodStart)
}

func validBillingStatus(status BillingStatus) bool {
	switch status {
	case BillingStatusPending, BillingStatusTrialing, BillingStatusActive,
		BillingStatusPastDue, BillingStatusCanceled, BillingStatusExpired:
		return true
	default:
		return false
	}
}

func validBillingText(value string, maxBytes int) bool {
	return value == "" || (utf8.ValidString(value) && len(value) <= maxBytes)
}
