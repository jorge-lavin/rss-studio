package policy

import "time"

// QuotaKind names one independent account quota.
type QuotaKind string

const (
	QuotaPublishedFeeds       QuotaKind = "published_feeds"
	QuotaPreviewsPerDay       QuotaKind = "previews_per_day"
	QuotaCreationsPerDay      QuotaKind = "creations_per_day"
	QuotaSplitsPerDay         QuotaKind = "splits_per_day"
	QuotaConcurrentOperations QuotaKind = "concurrent_expensive_operations"
)

// QuotaLimits are simple account counters. A zero limit deliberately denies
// that operation; negative limits are invalid configuration.
type QuotaLimits struct {
	MaxFeedsPerAccount               int
	PreviewsPerDay                   int
	CreationsPerDay                  int
	SplitOperationsPerDay            int
	MaxConcurrentExpensiveOperations int
}

const (
	DefaultMaxFeedsPerAccount               = 50
	DefaultPreviewsPerDay                   = 100
	DefaultCreationsPerDay                  = 20
	DefaultSplitOperationsPerDay            = 10
	DefaultMaxConcurrentExpensiveOperations = 2
)

func DefaultQuotaLimits() QuotaLimits {
	return QuotaLimits{
		MaxFeedsPerAccount:               DefaultMaxFeedsPerAccount,
		PreviewsPerDay:                   DefaultPreviewsPerDay,
		CreationsPerDay:                  DefaultCreationsPerDay,
		SplitOperationsPerDay:            DefaultSplitOperationsPerDay,
		MaxConcurrentExpensiveOperations: DefaultMaxConcurrentExpensiveOperations,
	}
}

func (limits QuotaLimits) Valid() bool {
	return limits.MaxFeedsPerAccount >= 0 &&
		limits.PreviewsPerDay >= 0 &&
		limits.CreationsPerDay >= 0 &&
		limits.SplitOperationsPerDay >= 0 &&
		limits.MaxConcurrentExpensiveOperations >= 0
}

// QuotaUsage describes one reservation. Daily counters use the reservation's
// UTC period; published feeds and concurrent operations are non-periodic.
type QuotaUsage struct {
	PublishedFeeds       int
	Previews             int
	Creations            int
	Splits               int
	ConcurrentOperations int
}

func (usage QuotaUsage) Valid() bool {
	return usage.PublishedFeeds >= 0 && usage.Previews >= 0 && usage.Creations >= 0 &&
		usage.Splits >= 0 && usage.ConcurrentOperations >= 0
}

func (usage QuotaUsage) Empty() bool {
	return usage == (QuotaUsage{})
}

// QuotaDecision describes an atomic reservation result.
type QuotaDecision struct {
	Allowed    bool
	Kind       QuotaKind
	Limit      int
	Remaining  int
	ResetAt    time.Time
	ResetAfter time.Duration
}

// QuotaReservation identifies counters that may be refunded after failed
// work. It contains no credentials or request data.
type QuotaReservation struct {
	AccountID   int64
	PeriodStart time.Time
	Usage       QuotaUsage
}

// QuotaStore persists account-scoped quota reservations atomically.
type QuotaStore interface {
	ReserveQuota(accountID int64, usage QuotaUsage, limits QuotaLimits, now time.Time) (QuotaDecision, QuotaReservation, error)
	RefundQuota(reservation QuotaReservation) error
}
