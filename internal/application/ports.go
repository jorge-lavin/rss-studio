package application

import (
	"context"
	"time"

	"rss-studio/internal/domain"
)

type FeedStore interface {
	List(owner domain.User) ([]domain.Feed, error)
	Get(path string) (domain.Feed, bool, error)
	Create(owner domain.User, name, sourceURL, keyword string) (domain.Feed, error)
	Delete(owner domain.User, path string) error
}

type OperatorFeedStore interface {
	CreateWithOperator(owner domain.User, name, sourceURL string, operator domain.FilterOperator, keyword string) (domain.Feed, error)
}

type FilterFeedStore interface {
	CreateWithFilters(owner domain.User, name, sourceURL string, match domain.FilterMatch, filters []domain.FilterCondition) (domain.Feed, error)
}

type Source interface {
	Head(ctx context.Context, sourceURL string) (time.Time, error)
	Get(ctx context.Context, sourceURL string) ([]byte, time.Time, error)
}

type CacheMetadata struct {
	LastModified time.Time
}

type FeedCache interface {
	Read(feed domain.Feed) ([]byte, error)
	ReadMetadata(feed domain.Feed) (CacheMetadata, error)
	Write(feed domain.Feed, data []byte, lastModified time.Time) error
	Remove(feed domain.Feed) error
}

type Filter func(source []byte, keyword, outputTitle string) ([]byte, int, error)

type OperatorFilter func(source []byte, operator domain.FilterOperator, keyword, outputTitle string) ([]byte, int, error)

type ConditionsFilter func(source []byte, match domain.FilterMatch, filters []domain.FilterCondition, outputTitle string) ([]byte, int, error)

type PreviewRule struct {
	Name     string
	Operator domain.FilterOperator
	Keyword  string
	Match    domain.FilterMatch
	Filters  []domain.FilterCondition
}

type PreviewResult struct {
	Name         string
	Operator     domain.FilterOperator
	Keyword      string
	Match        domain.FilterMatch
	Filters      []domain.FilterCondition
	Count        int
	Titles       []string
	FeedImageURL string
	Episodes     []PreviewEpisode
}

type PreviewEpisode struct {
	Title    string
	ImageURL string
}
