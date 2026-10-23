package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"rss-studio/internal/application"
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

func (c *FileCache) Read(slug string) ([]byte, error) {
	return os.ReadFile(c.feedPath(slug))
}

func (c *FileCache) ReadMetadata(slug string) (application.CacheMetadata, error) {
	data, err := os.ReadFile(c.metadataPath(slug))
	if err != nil {
		return application.CacheMetadata{}, err
	}
	var record metadataRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return application.CacheMetadata{}, err
	}
	return application.CacheMetadata{LastModified: record.LastModified}, nil
}

func (c *FileCache) Write(slug string, data []byte, lastModified time.Time) error {
	if err := os.MkdirAll(c.directory, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(c.feedPath(slug), data); err != nil {
		return err
	}
	metadata, err := json.Marshal(metadataRecord{LastModified: lastModified, GeneratedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return writeAtomic(c.metadataPath(slug), append(metadata, '\n'))
}

func (c *FileCache) Remove(slug string) error {
	for _, path := range []string{c.feedPath(slug), c.metadataPath(slug)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (c *FileCache) feedPath(slug string) string {
	return filepath.Join(c.directory, slug+".xml")
}

func (c *FileCache) metadataPath(slug string) string {
	return filepath.Join(c.directory, slug+".json")
}

func writeAtomic(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
