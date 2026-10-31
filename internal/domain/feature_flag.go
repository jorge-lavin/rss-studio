package domain

import (
	"errors"
	"time"
)

const (
	FeatureFlagBillingEnabled     = "billing.enabled"
	FeatureFlagBillingStripe      = "billing.stripe"
	FeatureFlagBillingBitcoin     = "billing.bitcoin"
	FeatureFlagBillingEnforcement = "billing.enforcement"
)

var (
	ErrFeatureFlagNotFound = errors.New("feature flag not found")
	ErrInvalidFeatureFlag  = errors.New("invalid feature flag")
)

var featureFlagNames = [...]string{
	FeatureFlagBillingEnabled,
	FeatureFlagBillingStripe,
	FeatureFlagBillingBitcoin,
	FeatureFlagBillingEnforcement,
}

type FeatureFlag struct {
	Name      string
	Enabled   bool
	UpdatedAt time.Time
	UpdatedBy *int64
}

func KnownFeatureFlags() []string {
	return append([]string(nil), featureFlagNames[:]...)
}

func IsKnownFeatureFlag(name string) bool {
	for _, knownName := range featureFlagNames {
		if name == knownName {
			return true
		}
	}
	return false
}
