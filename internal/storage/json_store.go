package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"rss-studio/internal/domain"
)

type JSONStore struct {
	mu    sync.RWMutex
	path  string
	feeds []domain.Feed
}

type feedRecord struct {
	Owner     string    `json:"owner,omitempty"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	SourceURL string    `json:"source_url"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"created_at"`
}

func NewJSONStore(path string) (*JSONStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	store := &JSONStore{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		var records []feedRecord
		if err := json.Unmarshal(data, &records); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		store.feeds = make([]domain.Feed, len(records))
		for i, record := range records {
			store.feeds[i] = fromRecord(record)
		}
	}
	return store, nil
}

func (s *JSONStore) List(owner string) []domain.Feed {
	s.mu.RLock()
	defer s.mu.RUnlock()
	feeds := make([]domain.Feed, 0, len(s.feeds))
	for _, feed := range s.feeds {
		if feed.Owner == owner {
			feeds = append(feeds, feed)
		}
	}
	sort.Slice(feeds, func(i, j int) bool {
		return feeds[i].CreatedAt.After(feeds[j].CreatedAt)
	})
	return feeds
}

func (s *JSONStore) Get(slug string) (domain.Feed, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, feed := range s.feeds {
		if feed.Slug == slug {
			return feed, true
		}
	}
	return domain.Feed{}, false
}

func (s *JSONStore) Create(owner, name, sourceURL, keyword string) (domain.Feed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	base := domain.Slugify(name)
	if base == "" {
		base = "feed"
	}
	slug := base
	for suffix := 2; s.slugExists(slug); suffix++ {
		slug = fmt.Sprintf("%s-%d", base, suffix)
	}

	feed := domain.Feed{
		Owner:     owner,
		Name:      name,
		Slug:      slug,
		SourceURL: sourceURL,
		Keyword:   keyword,
		CreatedAt: time.Now().UTC(),
	}
	s.feeds = append(s.feeds, feed)
	if err := s.save(); err != nil {
		s.feeds = s.feeds[:len(s.feeds)-1]
		return domain.Feed{}, err
	}
	return feed, nil
}

func (s *JSONStore) Delete(slug, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, feed := range s.feeds {
		if feed.Slug != slug || feed.Owner != owner {
			continue
		}
		original := append([]domain.Feed(nil), s.feeds...)
		s.feeds = append(s.feeds[:i], s.feeds[i+1:]...)
		if err := s.save(); err != nil {
			s.feeds = original
			return err
		}
		return nil
	}
	return os.ErrNotExist
}

func (s *JSONStore) slugExists(slug string) bool {
	for _, feed := range s.feeds {
		if feed.Slug == slug {
			return true
		}
	}
	return false
}

func (s *JSONStore) save() error {
	records := make([]feedRecord, len(s.feeds))
	for i, feed := range s.feeds {
		records[i] = toRecord(feed)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path, append(data, '\n'))
}

func writeAtomic(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func fromRecord(record feedRecord) domain.Feed {
	return domain.Feed{Owner: record.Owner, Name: record.Name, Slug: record.Slug, SourceURL: record.SourceURL, Keyword: record.Keyword, CreatedAt: record.CreatedAt}
}

func toRecord(feed domain.Feed) feedRecord {
	return feedRecord{Owner: feed.Owner, Name: feed.Name, Slug: feed.Slug, SourceURL: feed.SourceURL, Keyword: feed.Keyword, CreatedAt: feed.CreatedAt}
}
