package application

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"rss-studio/internal/domain"
)

var (
	ErrInvalidFeed       = errors.New("invalid feed")
	ErrFeedNotFound      = errors.New("feed not found")
	ErrCacheNotReady     = errors.New("feed cache not ready")
	ErrSourceUnavailable = errors.New("source unavailable")
)

const refreshCheckInterval = 5 * time.Minute

type Service struct {
	store          FeedStore
	cache          FeedCache
	source         Source
	filter         Filter
	operatorFilter OperatorFilter

	refreshMu  sync.Mutex
	stateMu    sync.Mutex
	refreshing map[string]bool
	checkedAt  map[string]time.Time
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
		refreshing:     make(map[string]bool),
		checkedAt:      make(map[string]time.Time),
	}
}

func (s *Service) ListFeeds(owner domain.User) ([]domain.Feed, error) {
	return s.store.List(owner)
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

func (s *Service) DeleteFeed(owner domain.User, path string) error {
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

func (s *Service) triggerRefresh(feed domain.Feed) {
	s.stateMu.Lock()
	if s.refreshing[feed.Path] || time.Since(s.checkedAt[feed.Path]) < refreshCheckInterval {
		s.stateMu.Unlock()
		return
	}
	s.refreshing[feed.Path] = true
	s.checkedAt[feed.Path] = time.Now()
	s.stateMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.refreshFeed(ctx, feed, false); err != nil {
			log.Printf("refresh %s: %v", feed.Path, err)
		}
		s.stateMu.Lock()
		delete(s.refreshing, feed.Path)
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
	parsed, err := url.ParseRequestURI(sourceURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: source URL must use http or https", ErrInvalidFeed)
	}
	return nil
}
