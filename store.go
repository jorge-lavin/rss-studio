package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type feedDefinition struct {
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	SourceURL string    `json:"source_url"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"created_at"`
}

type store struct {
	mu    sync.RWMutex
	path  string
	feeds []feedDefinition
}

func newStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	s := &store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.feeds); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return s, nil
}

func (s *store) list() []feedDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()

	feeds := append([]feedDefinition(nil), s.feeds...)
	sort.Slice(feeds, func(i, j int) bool {
		return feeds[i].CreatedAt.After(feeds[j].CreatedAt)
	})
	return feeds
}

func (s *store) get(slug string) (feedDefinition, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, feed := range s.feeds {
		if feed.Slug == slug {
			return feed, true
		}
	}
	return feedDefinition{}, false
}

func (s *store) create(name, sourceURL, keyword string) (feedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	base := slugify(name)
	if base == "" {
		base = "feed"
	}
	slug := base
	for suffix := 2; s.slugExists(slug); suffix++ {
		slug = fmt.Sprintf("%s-%d", base, suffix)
	}

	feed := feedDefinition{
		Name:      name,
		Slug:      slug,
		SourceURL: sourceURL,
		Keyword:   keyword,
		CreatedAt: time.Now().UTC(),
	}
	s.feeds = append(s.feeds, feed)
	if err := s.save(); err != nil {
		s.feeds = s.feeds[:len(s.feeds)-1]
		return feedDefinition{}, err
	}
	return feed, nil
}

func (s *store) delete(slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, feed := range s.feeds {
		if feed.Slug != slug {
			continue
		}
		removed := feed
		s.feeds = append(s.feeds[:i], s.feeds[i+1:]...)
		if err := s.save(); err != nil {
			s.feeds = append(s.feeds, feedDefinition{})
			copy(s.feeds[i+1:], s.feeds[i:])
			s.feeds[i] = removed
			return err
		}
		return nil
	}
	return os.ErrNotExist
}

func (s *store) slugExists(slug string) bool {
	for _, feed := range s.feeds {
		if feed.Slug == slug {
			return true
		}
	}
	return false
}

func (s *store) save() error {
	data, err := json.MarshalIndent(s.feeds, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

func slugify(value string) string {
	var result strings.Builder
	previousDash := false
	for _, char := range strings.ToLower(value) {
		switch {
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			result.WriteRune(char)
			previousDash = false
		case result.Len() > 0 && !previousDash:
			result.WriteByte('-')
			previousDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}
