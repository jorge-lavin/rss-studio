package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/idempotency"
	"rss-studio/internal/observability"
	"rss-studio/internal/policy"
	"rss-studio/internal/rss"
)

var (
	ErrInvalidFeed            = errors.New("invalid feed")
	ErrFeedNotFound           = errors.New("feed not found")
	ErrCacheNotReady          = errors.New("feed cache not ready")
	ErrSourceUnavailable      = errors.New("source unavailable")
	ErrIdempotencyUnavailable = errors.New("idempotency storage unavailable")
	ErrIdempotencyConflict    = idempotency.ErrConflict
	ErrQuotaExceeded          = errors.New("quota exceeded")
	ErrQuotaUnavailable       = errors.New("quota storage unavailable")
)

const (
	refreshCheckInterval = 5 * time.Minute
	// MaxSplitAggregateBytes bounds generated XML retained for one split.
	MaxSplitAggregateBytes = 128 << 20
)

type RefreshStatus string

const (
	RefreshStatusReady      RefreshStatus = "ready"
	RefreshStatusRefreshing RefreshStatus = "refreshing"
)

// MaxSplitOutputs limits work and persisted rows created by one split request.
const MaxSplitOutputs = 32

type Service struct {
	store            FeedStore
	cache            FeedCache
	source           Source
	filter           Filter
	operatorFilter   OperatorFilter
	conditionsFilter ConditionsFilter
	quotaLimits      policy.QuotaLimits

	refreshMu  sync.Mutex
	stateMu    sync.Mutex
	refreshing map[refreshKey]bool
	checkedAt  map[refreshKey]time.Time
}

type IdempotencyStore interface {
	ClaimIdempotency(ownerID int64, credentialHash, key, requestHash string, now time.Time) (idempotency.Claim, error)
	PrepareIdempotency(id int64, result []byte) error
	CompleteIdempotency(id int64, result []byte) error
	AbandonIdempotency(id int64) error
}

const idempotencyCompletionAttempts = 2

func completeIdempotency(store IdempotencyStore, id int64, result []byte) error {
	var err error
	for attempt := 0; attempt < idempotencyCompletionAttempts; attempt++ {
		if err = store.CompleteIdempotency(id, result); err == nil {
			return nil
		}
	}
	return err
}

type refreshKey struct {
	tenantID  int64
	sourceURL string
}

func NewService(store FeedStore, cache FeedCache, source Source, filter Filter) *Service {
	return newService(store, cache, source, filter, nil, nil, policy.DefaultQuotaLimits())
}

func NewServiceWithOperator(store FeedStore, cache FeedCache, source Source, filter OperatorFilter) *Service {
	return newService(store, cache, source, nil, filter, nil, policy.DefaultQuotaLimits())
}

func NewServiceWithConditions(store FeedStore, cache FeedCache, source Source, filter ConditionsFilter) *Service {
	return newService(store, cache, source, nil, nil, filter, policy.DefaultQuotaLimits())
}

// QuotaConfig is kept in the application package so callers do not need to
// know which policy package stores the shared defaults.
type QuotaConfig = policy.QuotaLimits

func DefaultQuotaConfig() QuotaConfig {
	return policy.DefaultQuotaLimits()
}

func NewServiceWithQuotas(store FeedStore, cache FeedCache, source Source, filter Filter, quotas QuotaConfig) *Service {
	return newService(store, cache, source, filter, nil, nil, quotas)
}

func NewServiceWithOperatorAndQuotas(store FeedStore, cache FeedCache, source Source, filter OperatorFilter, quotas QuotaConfig) *Service {
	return newService(store, cache, source, nil, filter, nil, quotas)
}

func NewServiceWithConditionsAndQuotas(store FeedStore, cache FeedCache, source Source, filter ConditionsFilter, quotas QuotaConfig) *Service {
	return newService(store, cache, source, nil, nil, filter, quotas)
}

func newService(store FeedStore, cache FeedCache, source Source, filter Filter, operatorFilter OperatorFilter, conditionsFilter ConditionsFilter, quotas policy.QuotaLimits) *Service {
	return &Service{
		store:            store,
		cache:            cache,
		source:           source,
		filter:           filter,
		operatorFilter:   operatorFilter,
		conditionsFilter: conditionsFilter,
		quotaLimits:      quotas,
		refreshing:       make(map[refreshKey]bool),
		checkedAt:        make(map[refreshKey]time.Time),
	}
}

// IdempotencyRequestHash returns a stable hash for one mutation operation and
// its decoded request. It stores neither request contents nor credentials.
func IdempotencyRequestHash(operation string, request any) (string, error) {
	data, err := json.Marshal(struct {
		Operation string `json:"operation"`
		Request   any    `json:"request"`
	}{Operation: operation, Request: request})
	if err != nil {
		return "", fmt.Errorf("hash idempotency request: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ExecuteIdempotent runs one mutation once and durably replays its JSON
// result for identical retries. Callers choose result representation so HTTP
// and future adapters can share this mechanism without sharing response code.
func (s *Service) ExecuteIdempotent(ctx context.Context, owner domain.User, credentialHash, key, requestHash string, operation func() (any, error)) ([]byte, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		result, err := operation()
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}
	if len(key) > 255 || owner.ID <= 0 || credentialHash == "" || requestHash == "" {
		return nil, ErrIdempotencyUnavailable
	}
	store, ok := s.store.(IdempotencyStore)
	if !ok {
		return nil, ErrIdempotencyUnavailable
	}

	var claim idempotency.Claim
	for {
		var err error
		claim, err = store.ClaimIdempotency(owner.ID, credentialHash, key, requestHash, time.Now().UTC())
		if errors.Is(err, idempotency.ErrConflict) {
			return nil, ErrIdempotencyConflict
		}
		if err != nil {
			return nil, fmt.Errorf("claim idempotency key: %w", err)
		}
		if claim.Completed {
			return append([]byte(nil), claim.Result...), nil
		}
		if claim.Acquired {
			break
		}
		if claim.Pending {
			if len(claim.Result) > 0 {
				if err := completeIdempotency(store, claim.ID, claim.Result); err != nil {
					return nil, fmt.Errorf("%w: complete idempotency key: %v", ErrIdempotencyUnavailable, err)
				}
				return append([]byte(nil), claim.Result...), nil
			}
			if claim.RecoveryRequired {
				return nil, ErrIdempotencyUnavailable
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		break
	}

	result, err := operation()
	if err != nil {
		if abandonErr := store.AbandonIdempotency(claim.ID); abandonErr != nil {
			return nil, errors.Join(err, fmt.Errorf("abandon idempotency key: %w", abandonErr))
		}
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("%w: encode idempotency result: %v", ErrIdempotencyUnavailable, err)
	}
	if err := store.PrepareIdempotency(claim.ID, encoded); err != nil {
		return nil, fmt.Errorf("%w: prepare idempotency result: %v", ErrIdempotencyUnavailable, err)
	}
	if err := completeIdempotency(store, claim.ID, encoded); err != nil {
		return nil, fmt.Errorf("%w: complete idempotency key: %v", ErrIdempotencyUnavailable, err)
	}
	return encoded, nil
}

func (s *Service) ListFeeds(owner domain.User) ([]domain.Feed, error) {
	return s.store.List(owner)
}

func (s *Service) RefreshStatus(feed domain.Feed) RefreshStatus {
	key := refreshKey{tenantID: feed.TenantID, sourceURL: feed.SourceURL}
	s.stateMu.Lock()
	refreshing := s.refreshing[key]
	s.stateMu.Unlock()
	if refreshing {
		return RefreshStatusRefreshing
	}
	return RefreshStatusReady
}

// RateLimitStore exposes the persistent policy counter when the backing store
// supports it. Adapters without persistent storage remain usable without rate
// limiting, which keeps service construction compatible with existing tests.
func (s *Service) RateLimitStore() policy.RateLimitStore {
	store, _ := s.store.(policy.RateLimitStore)
	return store
}

// QuotaError is safe to expose through an API response: it contains only
// quota metadata and never request, credential, or source data.
type QuotaError struct {
	Kind       policy.QuotaKind
	Limit      int
	Remaining  int
	ResetAt    time.Time
	ResetAfter time.Duration
}

func (e *QuotaError) Error() string { return "quota exceeded" }

func (e *QuotaError) Unwrap() error { return ErrQuotaExceeded }

func auditResult(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrQuotaExceeded):
		return "quota_rejected"
	case errors.Is(err, ErrSourceUnavailable):
		return "upstream_failure"
	default:
		return "error"
	}
}

type quotaLease struct {
	store       policy.QuotaStore
	reservation policy.QuotaReservation
}

func (l *quotaLease) finish(success bool) error {
	if l == nil || l.store == nil {
		return nil
	}
	reservation := l.reservation
	if success {
		reservation.Usage = policy.QuotaUsage{ConcurrentOperations: reservation.Usage.ConcurrentOperations}
	}
	if reservation.Usage.Empty() {
		return nil
	}
	return l.store.RefundQuota(reservation)
}

func (s *Service) reserveQuota(owner domain.User, usage policy.QuotaUsage) (*quotaLease, error) {
	store, ok := s.store.(policy.QuotaStore)
	if !ok || usage.Empty() {
		return nil, nil
	}
	accountID := owner.ID
	if accountID <= 0 && owner.Username != "" {
		if lookup, ok := s.store.(interface {
			User(string) (domain.User, error)
		}); ok {
			resolved, err := lookup.User(owner.Username)
			if err != nil {
				return nil, fmt.Errorf("resolve quota account: %w", err)
			}
			accountID = resolved.ID
		}
	}
	if accountID <= 0 {
		return nil, ErrQuotaUnavailable
	}
	decision, reservation, err := store.ReserveQuota(accountID, usage, s.quotaLimits, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrQuotaUnavailable, err)
	}
	if !decision.Allowed {
		return nil, &QuotaError{
			Kind: decision.Kind, Limit: decision.Limit, Remaining: decision.Remaining,
			ResetAt: decision.ResetAt, ResetAfter: decision.ResetAfter,
		}
	}
	return &quotaLease{store: store, reservation: reservation}, nil
}

func (s *Service) quotaFinished(lease *quotaLease, success bool) {
	if err := lease.finish(success); err != nil {
		log.Printf("finish quota reservation: %v", err)
	}
}

func (s *Service) Preview(ctx context.Context, sourceURL string, rules []PreviewRule) ([]PreviewResult, error) {
	return s.preview(ctx, domain.User{}, sourceURL, rules)
}

func (s *Service) PreviewForOwner(ctx context.Context, owner domain.User, sourceURL string, rules []PreviewRule) ([]PreviewResult, error) {
	return s.preview(ctx, owner, sourceURL, rules)
}

func (s *Service) preview(ctx context.Context, owner domain.User, sourceURL string, rules []PreviewRule) (previews []PreviewResult, err error) {
	ctx = observability.WithOperation(ctx, policy.OperationFeedsPreview)
	started := time.Now()
	defer func() {
		observability.Record(ctx, observability.Event{
			Event:           observability.EventOperation,
			Operation:       policy.OperationFeedsPreview,
			ActorID:         owner.ID,
			Result:          auditResult(err),
			DurationMS:      time.Since(started).Milliseconds(),
			QuotaRejected:   errors.Is(err, ErrQuotaExceeded),
			UpstreamFailure: errors.Is(err, ErrSourceUnavailable),
		})
	}()
	sourceURL = strings.TrimSpace(sourceURL)
	if err := validateSourceURL(sourceURL); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%w: at least one output is required", ErrInvalidFeed)
	}
	if len(rules) > MaxSplitOutputs {
		return nil, fmt.Errorf("%w: no more than %d outputs are allowed", ErrInvalidFeed, MaxSplitOutputs)
	}

	filterRules := make([]rss.FilterRule, len(rules))
	for i, rule := range rules {
		name := strings.TrimSpace(rule.Name)
		operator := domain.FilterOperator(strings.TrimSpace(string(rule.Operator)))
		keyword := strings.TrimSpace(rule.Keyword)
		match, filters, err := normalizeFilters(operator, keyword, rule.Match, rule.Filters)
		if err != nil {
			return nil, err
		}
		if err := validateFilterSet(name, sourceURL, match, filters); err != nil {
			return nil, err
		}
		filterRules[i] = rss.FilterRule{Name: name, Operator: filters[0].Operator, Keyword: filters[0].Keyword, Match: match, Filters: filters}
	}

	lease, err := s.reserveQuota(owner, policy.QuotaUsage{Previews: 1, ConcurrentOperations: 1})
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() { s.quotaFinished(lease, succeeded) }()

	sourceData, _, err := s.source.Get(ctx, sourceURL)
	if err != nil {
		observability.RecordFailure(ctx, "source.fetch", err)
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	filtered, err := rss.PreviewMany(sourceData, filterRules)
	if err != nil {
		observability.RecordFailure(ctx, "rss.preview", err)
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	previews = make([]PreviewResult, len(filtered))
	for i, result := range filtered {
		episodes := make([]PreviewEpisode, len(result.Episodes))
		for j, episode := range result.Episodes {
			episodes[j] = PreviewEpisode{
				Title:    episode.Title,
				ImageURL: previewImageURL(episode.ImageURL, sourceURL),
			}
		}
		previews[i] = PreviewResult{
			Name:         result.Name,
			Operator:     filterRules[i].Filters[0].Operator,
			Keyword:      filterRules[i].Filters[0].Keyword,
			Match:        filterRules[i].Match,
			Filters:      filterRules[i].Filters,
			Count:        result.Count,
			Titles:       result.Titles,
			FeedImageURL: previewImageURL(result.FeedImageURL, sourceURL),
			Episodes:     episodes,
		}
	}
	succeeded = true
	return previews, nil
}

func previewImageURL(value, sourceURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.String() == "" {
		return ""
	}
	if !parsed.IsAbs() {
		base, baseErr := url.Parse(sourceURL)
		if baseErr != nil {
			return ""
		}
		parsed = base.ResolveReference(parsed)
	}
	if parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return parsed.String()
}

func (s *Service) CreateFeed(ctx context.Context, owner domain.User, name, sourceURL, keyword string) (domain.Feed, error) {
	return s.CreateFeedWithOperator(ctx, owner, name, sourceURL, domain.FilterContains, keyword)
}

func (s *Service) CreateFeedWithOperator(ctx context.Context, owner domain.User, name, sourceURL string, operator domain.FilterOperator, keyword string) (feed domain.Feed, err error) {
	return s.CreateFeedWithFilters(ctx, owner, name, sourceURL, domain.FilterAny, []domain.FilterCondition{{Operator: operator, Keyword: keyword}})
}

func (s *Service) CreateFeedWithFilters(ctx context.Context, owner domain.User, name, sourceURL string, match domain.FilterMatch, filters []domain.FilterCondition) (feed domain.Feed, err error) {
	ctx = observability.WithOperation(ctx, policy.OperationFeedsCreate)
	started := time.Now()
	defer func() {
		observability.Record(ctx, observability.Event{
			Event:           observability.EventMutation,
			Operation:       policy.OperationFeedsCreate,
			ActorID:         owner.ID,
			Result:          auditResult(err),
			DurationMS:      time.Since(started).Milliseconds(),
			QuotaRejected:   errors.Is(err, ErrQuotaExceeded),
			UpstreamFailure: errors.Is(err, ErrSourceUnavailable),
		})
	}()
	name = strings.TrimSpace(name)
	sourceURL = strings.TrimSpace(sourceURL)
	match, filters, err = normalizeFilters("", "", match, filters)
	if err != nil {
		return domain.Feed{}, err
	}
	if err := validateFilterSet(name, sourceURL, match, filters); err != nil {
		return domain.Feed{}, err
	}
	operatorStore, hasOperatorStore := s.store.(OperatorFeedStore)
	filterStore, hasFilterStore := s.store.(FilterFeedStore)
	legacyStore := len(filters) == 1 && filters[0].Operator == domain.FilterContains
	if !hasFilterStore && !hasOperatorStore && !legacyStore {
		return domain.Feed{}, fmt.Errorf("%w: operator-aware feed storage is unavailable", ErrInvalidFeed)
	}
	lease, err := s.reserveQuota(owner, policy.QuotaUsage{PublishedFeeds: 1, Creations: 1, ConcurrentOperations: 1})
	if err != nil {
		return domain.Feed{}, err
	}
	succeeded := false
	defer func() { s.quotaFinished(lease, succeeded) }()

	if hasFilterStore {
		feed, err = filterStore.CreateWithFilters(owner, name, sourceURL, match, filters)
	} else if hasOperatorStore {
		feed, err = operatorStore.CreateWithOperator(owner, name, sourceURL, filters[0].Operator, filters[0].Keyword)
		feed.Match = match
		feed.Filters = filters
	} else if legacyStore {
		feed, err = s.store.Create(owner, name, sourceURL, filters[0].Keyword)
		feed.Match = match
		feed.Filters = filters
	} else {
		return domain.Feed{}, fmt.Errorf("%w: operator-aware feed storage is unavailable", ErrInvalidFeed)
	}
	if err != nil {
		observability.RecordFailure(ctx, "feed.store.create", err)
		return domain.Feed{}, err
	}
	if err := s.refreshFeed(ctx, feed, true); err != nil {
		observability.RecordFailure(ctx, "feed.refresh", err)
		if deleteErr := s.store.Delete(owner, feed.Path); deleteErr != nil {
			log.Printf("roll back feed: %T", deleteErr)
		}
		if removeErr := s.cache.Remove(feed); removeErr != nil {
			log.Printf("remove rolled-back cache: %T", removeErr)
		}
		return domain.Feed{}, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	succeeded = true
	return feed, nil
}

// CreateSplitFeeds publishes one filtered feed for each output using one source fetch.
func (s *Service) CreateSplitFeeds(ctx context.Context, owner domain.User, sourceURL string, rules []PreviewRule) (feeds []domain.Feed, err error) {
	ctx = observability.WithOperation(ctx, policy.OperationFeedsSplit)
	started := time.Now()
	defer func() {
		observability.Record(ctx, observability.Event{
			Event:           observability.EventMutation,
			Operation:       policy.OperationFeedsSplit,
			ActorID:         owner.ID,
			Result:          auditResult(err),
			DurationMS:      time.Since(started).Milliseconds(),
			QuotaRejected:   errors.Is(err, ErrQuotaExceeded),
			UpstreamFailure: errors.Is(err, ErrSourceUnavailable),
		})
	}()
	sourceURL = strings.TrimSpace(sourceURL)
	if err := validateSourceURL(sourceURL); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%w: at least one output is required", ErrInvalidFeed)
	}
	if len(rules) > MaxSplitOutputs {
		return nil, fmt.Errorf("%w: no more than %d outputs are allowed", ErrInvalidFeed, MaxSplitOutputs)
	}

	normalized := make([]PreviewRule, len(rules))
	filterRules := make([]rss.FilterRule, len(rules))
	for i, rule := range rules {
		name := strings.TrimSpace(rule.Name)
		operator := domain.FilterOperator(strings.TrimSpace(string(rule.Operator)))
		keyword := strings.TrimSpace(rule.Keyword)
		match, filters, err := normalizeFilters(operator, keyword, rule.Match, rule.Filters)
		if err != nil {
			return nil, err
		}
		if err := validateFilterSet(name, sourceURL, match, filters); err != nil {
			return nil, err
		}
		normalized[i] = PreviewRule{Name: name, Operator: filters[0].Operator, Keyword: filters[0].Keyword, Match: match, Filters: filters}
		filterRules[i] = rss.FilterRule{Name: name, Operator: filters[0].Operator, Keyword: filters[0].Keyword, Match: match, Filters: filters}
	}
	operatorStore, hasOperatorStore := s.store.(OperatorFeedStore)
	filterStore, hasFilterStore := s.store.(FilterFeedStore)
	legacyStore := true
	for _, rule := range normalized {
		if len(rule.Filters) != 1 || rule.Filters[0].Operator != domain.FilterContains {
			legacyStore = false
			break
		}
	}
	if !hasFilterStore && !hasOperatorStore && !legacyStore {
		return nil, fmt.Errorf("%w: operator-aware feed storage is unavailable", ErrInvalidFeed)
	}
	lease, err := s.reserveQuota(owner, policy.QuotaUsage{PublishedFeeds: len(normalized), Splits: 1, ConcurrentOperations: 1})
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() { s.quotaFinished(lease, succeeded) }()

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	sourceData, lastModified, err := s.source.Get(ctx, sourceURL)
	if err != nil {
		observability.RecordFailure(ctx, "source.fetch", err)
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if splitAggregateSizeExceeded(len(sourceData), len(filterRules)) {
		return nil, fmt.Errorf("%w: split output size limit exceeded", ErrInvalidFeed)
	}
	filtered, err := rss.FilterMany(sourceData, filterRules)
	if err != nil {
		observability.RecordFailure(ctx, "rss.filter", err)
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	generatedSize := 0
	for _, result := range filtered {
		if len(result.Feed) > MaxSplitAggregateBytes-generatedSize {
			return nil, fmt.Errorf("%w: split output size limit exceeded", ErrInvalidFeed)
		}
		generatedSize += len(result.Feed)
	}

	feeds = make([]domain.Feed, 0, len(filtered))
	for i, result := range filtered {
		var feed domain.Feed
		if hasFilterStore {
			feed, err = filterStore.CreateWithFilters(owner, normalized[i].Name, sourceURL, normalized[i].Match, normalized[i].Filters)
		} else if hasOperatorStore {
			feed, err = operatorStore.CreateWithOperator(owner, normalized[i].Name, sourceURL, normalized[i].Filters[0].Operator, normalized[i].Filters[0].Keyword)
			feed.Match = normalized[i].Match
			feed.Filters = normalized[i].Filters
		} else if legacyStore {
			feed, err = s.store.Create(owner, normalized[i].Name, sourceURL, normalized[i].Filters[0].Keyword)
			feed.Match = normalized[i].Match
			feed.Filters = normalized[i].Filters
		} else {
			return nil, withRollbackError(fmt.Errorf("%w: operator-aware feed storage is unavailable", ErrInvalidFeed), s.rollbackSplit(owner, feeds))
		}
		if err != nil {
			observability.RecordFailure(ctx, "feed.store.create", err)
			return nil, withRollbackError(err, s.rollbackSplit(owner, feeds))
		}
		feeds = append(feeds, feed)
		if err := s.cache.Write(feed, result.Feed, lastModified); err != nil {
			observability.RecordFailure(ctx, "feed.cache.write", err)
			return nil, withRollbackError(err, s.rollbackSplit(owner, feeds))
		}
	}
	succeeded = true
	return feeds, nil
}

func (s *Service) rollbackSplit(owner domain.User, feeds []domain.Feed) error {
	var rollbackErr error
	for i := len(feeds) - 1; i >= 0; i-- {
		feed := feeds[i]
		if err := s.cache.Remove(feed); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove feed cache: %w", err))
		}
		if err := s.store.Delete(owner, feed.Path); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("delete feed: %w", err))
		}
	}
	return rollbackErr
}

func withRollbackError(operationErr, rollbackErr error) error {
	if rollbackErr == nil {
		return operationErr
	}
	return errors.Join(operationErr, fmt.Errorf("split rollback failed: %w", rollbackErr))
}

// DeleteFeedContext is the context-aware form used by HTTP and other
// adapters so audit identity and request tracing reach the shared service.
func (s *Service) DeleteFeedContext(ctx context.Context, owner domain.User, path string) (err error) {
	started := time.Now()
	defer func() {
		observability.Record(ctx, observability.Event{
			Event:      observability.EventMutation,
			Operation:  policy.OperationFeedsDelete,
			ActorID:    owner.ID,
			Result:     auditResult(err),
			DurationMS: time.Since(started).Milliseconds(),
		})
	}()
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	accountID := owner.ID
	if accountID <= 0 {
		if lookup, ok := s.store.(interface {
			User(string) (domain.User, error)
		}); ok {
			resolved, err := lookup.User(owner.Username)
			if err != nil {
				return err
			}
			accountID = resolved.ID
		}
	}

	feed, found, err := s.store.Get(path)
	if err != nil {
		return err
	}
	if !found {
		return ErrFeedNotFound
	}
	if err := s.store.Delete(owner, path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrFeedNotFound
		}
		return err
	}
	if counter, ok := s.store.(interface{ DecrementPublishedFeedQuota(int64) error }); ok {
		if err := counter.DecrementPublishedFeedQuota(accountID); err != nil {
			return err
		}
	}
	return s.cache.Remove(feed)
}

func (s *Service) CachedFeed(ctx context.Context, path string) ([]byte, error) {
	feed, found, err := s.store.Get(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrFeedNotFound
	}

	data, err := s.cache.Read(feed)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.triggerRefresh(feed)
			return nil, ErrCacheNotReady
		}
		return nil, err
	}
	s.triggerRefresh(feed)
	return data, nil
}

func (s *Service) refreshFeed(ctx context.Context, feed domain.Feed, force bool) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	if !force {
		fresh, err := s.cacheIsFresh(ctx, feed)
		if err != nil {
			return err
		}
		if fresh {
			return nil
		}
	}

	sourceData, lastModified, err := s.source.Get(ctx, feed.SourceURL)
	if err != nil {
		return err
	}
	var filtered []byte
	var matched int
	if s.conditionsFilter != nil {
		filtered, matched, err = s.conditionsFilter(sourceData, feed.Match, feed.Filters, feed.Name)
	} else if s.operatorFilter != nil {
		filtered, matched, err = s.operatorFilter(sourceData, feed.Operator, feed.Keyword, feed.Name)
	} else {
		filtered, matched, err = s.filter(sourceData, feed.Keyword, feed.Name)
	}
	if err != nil {
		return err
	}
	if err := s.cache.Write(feed, filtered, lastModified); err != nil {
		return err
	}
	log.Printf("generated feed matching_episodes=%d", matched)
	return nil
}

func (s *Service) cacheIsFresh(ctx context.Context, feed domain.Feed) (bool, error) {
	if _, err := s.cache.Read(feed); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	metadata, err := s.cache.ReadMetadata(feed)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, nil
	}

	lastModified, err := s.source.Head(ctx, feed.SourceURL)
	if err != nil {
		return false, err
	}
	if lastModified.IsZero() {
		log.Printf("source returned no valid last_modified; keeping cached feed")
		return true, nil
	}
	if metadata.LastModified.IsZero() {
		return false, nil
	}
	changed := lastModified.After(metadata.LastModified)
	if changed {
		log.Printf("source changed")
	}
	return !changed, nil
}

func (s *Service) refreshFeedGroup(ctx context.Context, feed domain.Feed) error {
	ctx = observability.WithOperation(ctx, policy.OperationFeedsPublic)
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	feeds, err := s.store.List(domain.User{ID: feed.TenantID})
	if err != nil {
		return err
	}
	var triggering *domain.Feed
	for i := range feeds {
		if feeds[i].TenantID != feed.TenantID || feeds[i].Path != feed.Path {
			continue
		}
		if feeds[i].ID != feed.ID {
			return nil
		}
		triggering = &feeds[i]
		break
	}
	if triggering == nil {
		return nil
	}

	group := make([]domain.Feed, 0, len(feeds))
	for _, candidate := range feeds {
		if candidate.TenantID == triggering.TenantID && candidate.SourceURL == triggering.SourceURL {
			group = append(group, candidate)
		}
	}

	needsRefresh := false
	metadata := make([]CacheMetadata, len(group))
	for i, candidate := range group {
		if _, err := s.cache.Read(candidate); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				needsRefresh = true
				continue
			}
			return err
		}
		metadata[i], err = s.cache.ReadMetadata(candidate)
		if err != nil {
			needsRefresh = true
			continue
		}
	}

	lastModified, err := s.source.Head(ctx, triggering.SourceURL)
	if err != nil {
		return err
	}
	if !needsRefresh {
		if lastModified.IsZero() {
			log.Printf("source returned no valid last_modified; keeping cached feed")
			return nil
		}
		for _, cached := range metadata {
			if cached.LastModified.IsZero() || lastModified.After(cached.LastModified) {
				needsRefresh = true
				break
			}
		}
	}
	if !needsRefresh {
		return nil
	}

	sourceData, lastModified, err := s.source.Get(ctx, triggering.SourceURL)
	if err != nil {
		return err
	}
	rules := make([]rss.FilterRule, len(group))
	for i, candidate := range group {
		rules[i] = rss.FilterRule{Name: candidate.Name, Operator: candidate.Operator, Keyword: candidate.Keyword, Match: candidate.Match, Filters: candidate.Filters}
	}
	filtered, err := rss.FilterMany(sourceData, rules)
	if err != nil {
		return err
	}
	for i, candidate := range group {
		if err := s.cache.Write(candidate, filtered[i].Feed, lastModified); err != nil {
			return err
		}
		log.Printf("generated feed matching_episodes=%d", filtered[i].Count)
	}
	return nil
}

func splitAggregateSizeExceeded(sourceSize, outputCount int) bool {
	return outputCount > 0 && sourceSize > MaxSplitAggregateBytes/outputCount
}

func (s *Service) triggerRefresh(feed domain.Feed) {
	key := refreshKey{tenantID: feed.TenantID, sourceURL: feed.SourceURL}
	s.stateMu.Lock()
	if s.refreshing[key] || time.Since(s.checkedAt[key]) < refreshCheckInterval {
		s.stateMu.Unlock()
		return
	}
	s.refreshing[key] = true
	s.checkedAt[key] = time.Now()
	s.stateMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		refreshErr := s.refreshFeedGroup(ctx, feed)
		if refreshErr != nil {
			log.Printf("refresh feed: %T", refreshErr)
		}
		s.stateMu.Lock()
		if refreshErr != nil {
			delete(s.checkedAt, key)
		}
		delete(s.refreshing, key)
		s.stateMu.Unlock()
	}()
}

func validateFeed(name, sourceURL string, operator domain.FilterOperator, keyword string) error {
	return validateFilterSet(name, sourceURL, domain.FilterAny, []domain.FilterCondition{{Operator: operator, Keyword: keyword}})
}

const MaxFilterConditions = 32

func normalizeFilters(operator domain.FilterOperator, keyword string, match domain.FilterMatch, filters []domain.FilterCondition) (domain.FilterMatch, []domain.FilterCondition, error) {
	if len(filters) == 0 {
		if operator == "" {
			operator = domain.FilterContains
		}
		filters = []domain.FilterCondition{{Operator: operator, Keyword: keyword}}
	}
	if match == "" {
		match = domain.FilterAny
	}
	match = domain.FilterMatch(strings.TrimSpace(string(match)))
	normalized := make([]domain.FilterCondition, len(filters))
	for i, filter := range filters {
		operator := domain.FilterOperator(strings.TrimSpace(string(filter.Operator)))
		if operator == "" {
			operator = domain.FilterContains
		}
		normalized[i] = domain.FilterCondition{
			Operator: operator,
			Keyword:  strings.TrimSpace(filter.Keyword),
		}
	}
	return match, normalized, nil
}

func validateFilterSet(name, sourceURL string, match domain.FilterMatch, filters []domain.FilterCondition) error {
	if name == "" || sourceURL == "" || len(filters) == 0 {
		return fmt.Errorf("%w: name, source URL, and at least one filter are required", ErrInvalidFeed)
	}
	if !match.Valid() {
		return fmt.Errorf("%w: unsupported filter combination", ErrInvalidFeed)
	}
	if len(filters) > MaxFilterConditions {
		return fmt.Errorf("%w: no more than %d filters are allowed", ErrInvalidFeed, MaxFilterConditions)
	}
	if len(name) > 120 || len(sourceURL) > 2048 {
		return fmt.Errorf("%w: one or more fields are too long", ErrInvalidFeed)
	}
	for _, filter := range filters {
		if filter.Operator == "" || filter.Keyword == "" {
			return fmt.Errorf("%w: filter operator and keyword are required", ErrInvalidFeed)
		}
		if !filter.Operator.Valid() {
			return fmt.Errorf("%w: unsupported filter operator", ErrInvalidFeed)
		}
		if len(filter.Keyword) > 200 {
			return fmt.Errorf("%w: one or more fields are too long", ErrInvalidFeed)
		}
	}
	return validateSourceURL(sourceURL)
}

func validateSourceURL(sourceURL string) error {
	if sourceURL == "" {
		return fmt.Errorf("%w: source URL is required", ErrInvalidFeed)
	}
	if len(sourceURL) > 2048 {
		return fmt.Errorf("%w: source URL is too long", ErrInvalidFeed)
	}
	parsed, err := url.ParseRequestURI(sourceURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: source URL must use http or https", ErrInvalidFeed)
	}
	return nil
}
