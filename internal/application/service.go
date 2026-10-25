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
	store  FeedStore
	cache  FeedCache
	source Source
	filter Filter

	refreshMu  sync.Mutex
	stateMu    sync.Mutex
	refreshing map[string]bool
	checkedAt  map[string]time.Time
}

func NewService(store FeedStore, cache FeedCache, source Source, filter Filter) *Service {
	return &Service{
		store:      store,
		cache:      cache,
		source:     source,
		filter:     filter,
		refreshing: make(map[string]bool),
		checkedAt:  make(map[string]time.Time),
	}
}

func (s *Service) ListFeeds(owner domain.User) ([]domain.Feed, error) {
	return s.store.List(owner)
}

func (s *Service) CreateFeed(ctx context.Context, owner domain.User, name, sourceURL, keyword string) (domain.Feed, error) {
	name = strings.TrimSpace(name)
	sourceURL = strings.TrimSpace(sourceURL)
	keyword = strings.TrimSpace(keyword)
	if err := validateFeed(name, sourceURL, keyword); err != nil {
		return domain.Feed{}, err
	}

	feed, err := s.store.Create(owner, name, sourceURL, keyword)
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
	filtered, matched, err := s.filter(sourceData, feed.Keyword, feed.Name)
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

func validateFeed(name, sourceURL, keyword string) error {
	if name == "" || sourceURL == "" || keyword == "" {
		return fmt.Errorf("%w: name, source URL, and keyword are required", ErrInvalidFeed)
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
