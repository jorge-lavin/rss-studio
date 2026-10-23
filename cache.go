package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const maxFeedSize = 25 << 20
const refreshCheckInterval = 5 * time.Minute

type cacheMetadata struct {
	LastModified time.Time `json:"last_modified"`
	GeneratedAt  time.Time `json:"generated_at"`
}

func (a *app) triggerRefresh(feed feedDefinition) {
	a.stateMu.Lock()
	if a.refreshing[feed.Slug] || time.Since(a.checkedAt[feed.Slug]) < refreshCheckInterval {
		a.stateMu.Unlock()
		return
	}
	a.refreshing[feed.Slug] = true
	a.checkedAt[feed.Slug] = time.Now()
	a.stateMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.refreshFeed(ctx, feed, false); err != nil {
			log.Printf("refresh %s: %v", feed.Slug, err)
		}
		a.stateMu.Lock()
		delete(a.refreshing, feed.Slug)
		a.stateMu.Unlock()
	}()
}

func (a *app) refreshFeed(ctx context.Context, feed feedDefinition, force bool) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	if !force {
		fresh, err := a.cacheIsFresh(ctx, feed)
		if err != nil {
			return err
		}
		if fresh {
			return nil
		}
	}

	source, lastModified, err := a.downloadSource(ctx, feed.SourceURL)
	if err != nil {
		return err
	}
	filtered, matched, err := filterRSS(source, feed.Keyword, feed.Name)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(a.cacheDir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(a.feedCachePath(feed.Slug), filtered); err != nil {
		return err
	}
	metadata, err := json.Marshal(cacheMetadata{
		LastModified: lastModified,
		GeneratedAt:  time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if err := writeAtomic(a.metadataCachePath(feed.Slug), append(metadata, '\n')); err != nil {
		return err
	}
	log.Printf("generated %s with %d matching episodes", feed.Slug, matched)
	return nil
}

func (a *app) cacheIsFresh(ctx context.Context, feed feedDefinition) (bool, error) {
	if _, err := os.Stat(a.feedCachePath(feed.Slug)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	data, err := os.ReadFile(a.metadataCachePath(feed.Slug))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var metadata cacheMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return false, nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodHead, feed.SourceURL, nil)
	if err != nil {
		return false, err
	}
	setFeedRequestHeaders(request)
	response, err := a.client.Do(request)
	if err != nil {
		return false, err
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("source HEAD returned %s", response.Status)
	}

	lastModifiedHeader := response.Header.Get("Last-Modified")
	lastModified, err := http.ParseTime(lastModifiedHeader)
	if err != nil {
		log.Printf("source %s returned no valid Last-Modified; refreshing feed", feed.Slug)
		return false, nil
	}
	if metadata.LastModified.IsZero() {
		log.Printf("cache %s has no Last-Modified value", feed.Slug)
		return false, nil
	}
	changed := lastModified.After(metadata.LastModified)
	if changed {
		log.Printf("source %s changed: %s -> %s", feed.Slug, metadata.LastModified.Format(http.TimeFormat), lastModified.Format(http.TimeFormat))
	}
	return !changed, nil
}

func (a *app) downloadSource(ctx context.Context, sourceURL string) ([]byte, time.Time, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	setFeedRequestHeaders(request)
	response, err := a.client.Do(request)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, time.Time{}, fmt.Errorf("source GET returned %s", response.Status)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxFeedSize+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(data) > maxFeedSize {
		return nil, time.Time{}, fmt.Errorf("source feed exceeds %d bytes", maxFeedSize)
	}
	lastModified, _ := http.ParseTime(response.Header.Get("Last-Modified"))
	return data, lastModified, nil
}

func setFeedRequestHeaders(request *http.Request) {
	request.Header.Set("User-Agent", "RSS-Studio/0.1")
	request.Header.Set("Accept", "application/rss+xml, application/xml, text/xml;q=0.9, */*;q=0.1")
}

func (a *app) feedCachePath(slug string) string {
	return filepath.Join(a.cacheDir, slug+".xml")
}

func (a *app) metadataCachePath(slug string) string {
	return filepath.Join(a.cacheDir, slug+".json")
}

func (a *app) removeCachedFeed(slug string) {
	for _, path := range []string{a.feedCachePath(slug), a.metadataCachePath(slug)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("remove cache %s: %v", path, err)
		}
	}
}

func writeAtomic(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
