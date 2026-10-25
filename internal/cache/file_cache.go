package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"rss-studio/internal/application"
	"rss-studio/internal/domain"
)

type FileCache struct {
	directory string
}

type metadataRecord struct {
	LastModified time.Time `json:"last_modified"`
	GeneratedAt  time.Time `json:"generated_at"`
}

func NewFileCache(directory string) *FileCache {
	return &FileCache{directory: directory}
}

func (c *FileCache) Read(feed domain.Feed) ([]byte, error) {
	path, err := c.feedPath(feed)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (c *FileCache) ReadMetadata(feed domain.Feed) (application.CacheMetadata, error) {
	path, err := c.metadataPath(feed)
	if err != nil {
		return application.CacheMetadata{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return application.CacheMetadata{}, err
	}
	var record metadataRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return application.CacheMetadata{}, err
	}
	return application.CacheMetadata{LastModified: record.LastModified}, nil
}

func (c *FileCache) Write(feed domain.Feed, data []byte, lastModified time.Time) error {
	feedPath, err := c.feedPath(feed)
	if err != nil {
		return err
	}
	metadataPath, err := c.metadataPath(feed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(feedPath), 0o750); err != nil {
		return err
	}
	if err := writeAtomic(feedPath, data); err != nil {
		return err
	}
	metadata, err := json.Marshal(metadataRecord{LastModified: lastModified, GeneratedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return writeAtomic(metadataPath, append(metadata, '\n'))
}

func (c *FileCache) Remove(feed domain.Feed) error {
	feedPath, err := c.feedPath(feed)
	if err != nil {
		return err
	}
	metadataPath, err := c.metadataPath(feed)
	if err != nil {
		return err
	}
	for _, path := range []string{feedPath, metadataPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (c *FileCache) feedPath(feed domain.Feed) (string, error) {
	if feed.TenantID <= 0 {
		return "", fmt.Errorf("invalid feed tenant")
	}
	if feed.Path == "" || filepath.Base(feed.Path) != feed.Path || feed.Path == "." || feed.Path == ".." {
		return "", fmt.Errorf("invalid feed path")
	}
	return filepath.Join(c.directory, strconv.FormatInt(feed.TenantID, 10), feed.Path+".xml"), nil
}

func (c *FileCache) metadataPath(feed domain.Feed) (string, error) {
	if feed.TenantID <= 0 {
		return "", fmt.Errorf("invalid feed tenant")
	}
	if feed.Path == "" || filepath.Base(feed.Path) != feed.Path || feed.Path == "." || feed.Path == ".." {
		return "", fmt.Errorf("invalid feed path")
	}
	return filepath.Join(c.directory, strconv.FormatInt(feed.TenantID, 10), feed.Path+".json"), nil
}

func writeAtomic(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
