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
)

const (
	refreshCheckInterval = 5 * time.Minute
	// MaxSplitAggregateBytes bounds generated XML retained for one split.
	MaxSplitAggregateBytes = 128 << 20
)

// MaxSplitOutputs limits work and persisted rows created by one split request.
const MaxSplitOutputs = 32

type Service struct {
	store          FeedStore
	cache          FeedCache
	source         Source
	filter         Filter
	operatorFilter OperatorFilter

	refreshMu  sync.Mutex
	stateMu    sync.Mutex
	refreshing map[refreshKey]bool
	checkedAt  map[refreshKey]time.Time
}

type IdempotencyStore interface {
	ClaimIdempotency(ownerID int64, credentialHash, key, requestHash string, now time.Time) (idempotency.Claim, error)
	CompleteIdempotency(id int64, result []byte) error
	AbandonIdempotency(id int64) error
}

type refreshKey struct {
	tenantID  int64
	sourceURL string
}

func NewService(store FeedStore, cache FeedCache, source Source, filter Filter) *Service {
	return newService(store, cache, source, filter, nil)
}

func NewServiceWithOperator(store FeedStore, cache FeedCache, source Source, filter OperatorFilter) *Service {
	return newService(store, cache, source, nil, filter)
}

func newService(store FeedStore, cache FeedCache, source Source, filter Filter, operatorFilter OperatorFilter) *Service {
	return &Service{
		store:          store,
		cache:          cache,
		source:         source,
		filter:         filter,
		operatorFilter: operatorFilter,
		refreshing:     make(map[refreshKey]bool),
		checkedAt:      make(map[refreshKey]time.Time),
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
		_ = store.AbandonIdempotency(claim.ID)
		return nil, fmt.Errorf("encode idempotency result: %w", err)
	}
	if err := store.CompleteIdempotency(claim.ID, encoded); err != nil {
		return nil, fmt.Errorf("complete idempotency key: %w", err)
	}
	return encoded, nil
}

func (s *Service) ListFeeds(owner domain.User) ([]domain.Feed, error) {
	return s.store.List(owner)
}

// RateLimitStore exposes the persistent policy counter when the backing store
// supports it. Adapters without persistent storage remain usable without rate
// limiting, which keeps service construction compatible with existing tests.
func (s *Service) RateLimitStore() policy.RateLimitStore {
	store, _ := s.store.(policy.RateLimitStore)
	return store
}

func (s *Service) Preview(ctx context.Context, sourceURL string, rules []PreviewRule) ([]PreviewResult, error) {
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
		if operator == "" {
			operator = domain.FilterContains
		}
		if err := validateFeed(name, sourceURL, operator, keyword); err != nil {
			return nil, err
		}
		filterRules[i] = rss.FilterRule{Name: name, Operator: operator, Keyword: keyword}
	}

	sourceData, _, err := s.source.Get(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	filtered, err := rss.PreviewMany(sourceData, filterRules)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	previews := make([]PreviewResult, len(filtered))
	for i, result := range filtered {
		previews[i] = PreviewResult{
			Name:     result.Name,
			Operator: filterRules[i].Operator,
			Keyword:  filterRules[i].Keyword,
			Count:    result.Count,
			Titles:   result.Titles,
		}
	}
	return previews, nil
}

func (s *Service) CreateFeed(ctx context.Context, owner domain.User, name, sourceURL, keyword string) (domain.Feed, error) {
	return s.CreateFeedWithOperator(ctx, owner, name, sourceURL, domain.FilterContains, keyword)
}

func (s *Service) CreateFeedWithOperator(ctx context.Context, owner domain.User, name, sourceURL string, operator domain.FilterOperator, keyword string) (domain.Feed, error) {
	name = strings.TrimSpace(name)
	sourceURL = strings.TrimSpace(sourceURL)
	operator = domain.FilterOperator(strings.TrimSpace(string(operator)))
	keyword = strings.TrimSpace(keyword)
	if operator == "" {
		operator = domain.FilterContains
	}
	if err := validateFeed(name, sourceURL, operator, keyword); err != nil {
		return domain.Feed{}, err
	}

	var feed domain.Feed
	var err error
	if operatorStore, ok := s.store.(OperatorFeedStore); ok {
		feed, err = operatorStore.CreateWithOperator(owner, name, sourceURL, operator, keyword)
	} else if operator == domain.FilterContains {
		feed, err = s.store.Create(owner, name, sourceURL, keyword)
		feed.Operator = operator
	} else {
		return domain.Feed{}, fmt.Errorf("%w: operator-aware feed storage is unavailable", ErrInvalidFeed)
	}
	if err != nil {
		return domain.Feed{}, err
	}
	if err := s.refreshFeed(ctx, feed, true); err != nil {
		if deleteErr := s.store.Delete(owner, feed.Path); deleteErr != nil {
			log.Printf("roll back %s: %v", feed.Path, deleteErr)
		}
		if removeErr := s.cache.Remove(feed); removeErr != nil {
			log.Printf("remove rolled-back cache %s: %v", feed.Path, removeErr)
		}
		return domain.Feed{}, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	return feed, nil
}

// CreateSplitFeeds publishes one filtered feed for each output using one source fetch.
func (s *Service) CreateSplitFeeds(ctx context.Context, owner domain.User, sourceURL string, rules []PreviewRule) ([]domain.Feed, error) {
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

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	normalized := make([]PreviewRule, len(rules))
	filterRules := make([]rss.FilterRule, len(rules))
	for i, rule := range rules {
		name := strings.TrimSpace(rule.Name)
		operator := domain.FilterOperator(strings.TrimSpace(string(rule.Operator)))
		keyword := strings.TrimSpace(rule.Keyword)
		if operator == "" {
			operator = domain.FilterContains
		}
		if err := validateFeed(name, sourceURL, operator, keyword); err != nil {
			return nil, err
		}
		normalized[i] = PreviewRule{Name: name, Operator: operator, Keyword: keyword}
		filterRules[i] = rss.FilterRule{Name: name, Operator: operator, Keyword: keyword}
	}

	sourceData, lastModified, err := s.source.Get(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if splitAggregateSizeExceeded(len(sourceData), len(filterRules)) {
		return nil, fmt.Errorf("%w: split output size limit exceeded", ErrInvalidFeed)
	}
	filtered, err := rss.FilterMany(sourceData, filterRules)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	generatedSize := 0
	for _, result := range filtered {
		if len(result.Feed) > MaxSplitAggregateBytes-generatedSize {
			return nil, fmt.Errorf("%w: split output size limit exceeded", ErrInvalidFeed)
		}
		generatedSize += len(result.Feed)
	}

	feeds := make([]domain.Feed, 0, len(filtered))
	for i, result := range filtered {
		var feed domain.Feed
		if operatorStore, ok := s.store.(OperatorFeedStore); ok {
			feed, err = operatorStore.CreateWithOperator(owner, normalized[i].Name, sourceURL, normalized[i].Operator, normalized[i].Keyword)
		} else if normalized[i].Operator == domain.FilterContains {
			feed, err = s.store.Create(owner, normalized[i].Name, sourceURL, normalized[i].Keyword)
			feed.Operator = normalized[i].Operator
		} else {
			return nil, withRollbackError(fmt.Errorf("%w: operator-aware feed storage is unavailable", ErrInvalidFeed), s.rollbackSplit(owner, feeds))
		}
		if err != nil {
			return nil, withRollbackError(err, s.rollbackSplit(owner, feeds))
		}
		feeds = append(feeds, feed)
		if err := s.cache.Write(feed, result.Feed, lastModified); err != nil {
			return nil, withRollbackError(err, s.rollbackSplit(owner, feeds))
		}
	}
	return feeds, nil
}

func (s *Service) rollbackSplit(owner domain.User, feeds []domain.Feed) error {
	var rollbackErr error
	for i := len(feeds) - 1; i >= 0; i-- {
		feed := feeds[i]
		if err := s.cache.Remove(feed); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove cache for %s: %w", feed.Path, err))
		}
		if err := s.store.Delete(owner, feed.Path); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("delete feed %s: %w", feed.Path, err))
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

func (s *Service) DeleteFeed(owner domain.User, path string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

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
	if s.operatorFilter != nil {
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
	log.Printf("generated %s with %d matching episodes", feed.Path, matched)
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
		log.Printf("source %s returned no valid Last-Modified; keeping cached feed", feed.Slug)
		return true, nil
	}
	if metadata.LastModified.IsZero() {
		return false, nil
	}
	changed := lastModified.After(metadata.LastModified)
	if changed {
		log.Printf("source %s changed: %s -> %s", feed.Slug, metadata.LastModified.Format(time.RFC1123), lastModified.Format(time.RFC1123))
	}
	return !changed, nil
}

func (s *Service) refreshFeedGroup(ctx context.Context, feed domain.Feed) error {
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
			log.Printf("source %s returned no valid Last-Modified; keeping cached feed", triggering.Slug)
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
		rules[i] = rss.FilterRule{Name: candidate.Name, Operator: candidate.Operator, Keyword: candidate.Keyword}
	}
	filtered, err := rss.FilterMany(sourceData, rules)
	if err != nil {
		return err
	}
	for i, candidate := range group {
		if err := s.cache.Write(candidate, filtered[i].Feed, lastModified); err != nil {
			return err
		}
		log.Printf("generated %s with %d matching episodes", candidate.Path, filtered[i].Count)
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
			log.Printf("refresh %s: %v", feed.Path, refreshErr)
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
	if name == "" || sourceURL == "" || operator == "" || keyword == "" {
		return fmt.Errorf("%w: name, source URL, operator, and keyword are required", ErrInvalidFeed)
	}
	if !operator.Valid() {
		return fmt.Errorf("%w: unsupported filter operator", ErrInvalidFeed)
	}
	if len(name) > 120 || len(sourceURL) > 2048 || len(keyword) > 200 {
		return fmt.Errorf("%w: one or more fields are too long", ErrInvalidFeed)
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
