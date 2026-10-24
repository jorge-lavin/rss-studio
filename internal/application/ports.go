package application

import (
	"context"
	"time"

	"rss-studio/internal/domain"
)

type FeedStore interface {
	List(owner string) []domain.Feed
	Get(slug string) (domain.Feed, bool)
	Create(owner, name, sourceURL, keyword string) (domain.Feed, error)
	Delete(slug, owner string) error
}

type Source interface {
	Head(ctx context.Context, sourceURL string) (time.Time, error)
	Get(ctx context.Context, sourceURL string) ([]byte, time.Time, error)
}

type CacheMetadata struct {
	LastModified time.Time
}

type FeedCache interface {
	Read(slug string) ([]byte, error)
	ReadMetadata(slug string) (CacheMetadata, error)
	Write(slug string, data []byte, lastModified time.Time) error
	Remove(slug string) error
}

type Filter func(source []byte, keyword, outputTitle string) ([]byte, int, error)
