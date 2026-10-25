package storage

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"rss-studio/internal/domain"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	ErrUserNotFound      = errors.New("user not found")
	ErrInvalidCredential = errors.New("invalid credentials")
)

type SQLiteStore struct {
	db *sql.DB
}

type legacyFeedRecord struct {
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	SourceURL string    `json:"source_url"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"created_at"`
}

type migration struct {
	version int
	name    string
	sql     string
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("sqlite database path is required")
	}
	if err := prepareDatabaseFile(path); err != nil {
		return nil, fmt.Errorf("prepare sqlite database: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = db.Close()
		}
	}()

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return nil, fmt.Errorf("enable sqlite foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		return nil, fmt.Errorf("set sqlite busy timeout: %w", err)
	}

	store := &SQLiteStore{db: db}
	if err := store.Migrate(); err != nil {
		return nil, err
	}
	closeOnError = false
	return store, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) Migrate() error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}

	var current int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if len(migrations) == 0 {
		return errors.New("no sqlite migrations found")
	}
	if current > migrations[len(migrations)-1].version {
		return fmt.Errorf("sqlite database schema version %d is newer than supported version %d", current, migrations[len(migrations)-1].version)
	}

	for _, migration := range migrations {
		if migration.version <= current {
			continue
		}
		if migration.version != current+1 {
			return fmt.Errorf("missing sqlite migration %d", current+1)
		}

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin sqlite migration %d: %w", migration.version, err)
		}
		if _, err := tx.Exec(migration.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply sqlite migration %d: %w", migration.version, err)
		}
		if _, err := tx.Exec(
			"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
			migration.version,
			migration.name,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record sqlite migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit sqlite migration %d: %w", migration.version, err)
		}
		current = migration.version
	}
	return nil
}

func (s *SQLiteStore) SchemaVersion() (int, error) {
	var version int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func (s *SQLiteStore) CreateUser(username, password string) (domain.User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return domain.User{}, fmt.Errorf("%w: username and password are required", domain.ErrInvalidUser)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return domain.User{}, fmt.Errorf("%w: password cannot be stored", domain.ErrInvalidUser)
	}
	createdAt := time.Now().UTC()
	result, err := s.db.Exec(`
		INSERT INTO users (username, password_hash, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(username) DO NOTHING
	`, username, string(hash), createdAt.Format(time.RFC3339Nano))
	if err != nil {
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return domain.User{}, fmt.Errorf("check created user: %w", err)
	}
	if rows == 0 {
		return domain.User{}, domain.ErrUsernameTaken
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.User{}, fmt.Errorf("read created user: %w", err)
	}
	return domain.User{ID: id, Username: username, PasswordHash: string(hash), CreatedAt: createdAt}, nil
}

func (s *SQLiteStore) User(username string) (domain.User, error) {
	username = strings.TrimSpace(username)
	var user domain.User
	var createdAt string
	if err := s.db.QueryRow(
		"SELECT id, username, password_hash, created_at FROM users WHERE username = ?",
		username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, ErrUserNotFound
		}
		return domain.User{}, fmt.Errorf("read user: %w", err)
	}
	var err error
	user.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.User{}, fmt.Errorf("parse user creation time: %w", err)
	}
	return user, nil
}

func (s *SQLiteStore) FirstUser() (domain.User, error) {
	var username string
	if err := s.db.QueryRow("SELECT username FROM users ORDER BY id LIMIT 1").Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, ErrUserNotFound
		}
		return domain.User{}, fmt.Errorf("read first user: %w", err)
	}
	return s.User(username)
}

func (s *SQLiteStore) Authenticate(username, password string) (domain.User, error) {
	user, err := s.User(username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return domain.User{}, ErrInvalidCredential
		}
		return domain.User{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return domain.User{}, ErrInvalidCredential
	}
	return user, nil
}

func (s *SQLiteStore) List(owner domain.User) ([]domain.Feed, error) {
	userID, err := s.ownerID(owner)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT id, user_id, name, slug, path, source_url, filter_operator, keyword, created_at
		FROM feeds
		WHERE user_id = ?
		ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list feeds: %w", err)
	}
	defer rows.Close()

	feeds := make([]domain.Feed, 0)
	for rows.Next() {
		feed, err := scanFeed(rows)
		if err != nil {
			return nil, fmt.Errorf("read feed: %w", err)
		}
		feeds = append(feeds, feed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feeds: %w", err)
	}
	return feeds, nil
}

func (s *SQLiteStore) Get(path string) (domain.Feed, bool, error) {
	feed, err := scanFeed(s.db.QueryRow(`
		SELECT id, user_id, name, slug, path, source_url, filter_operator, keyword, created_at
		FROM feeds
		WHERE path = ?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Feed{}, false, nil
	}
	if err != nil {
		return domain.Feed{}, false, fmt.Errorf("read feed: %w", err)
	}
	return feed, true, nil
}

func (s *SQLiteStore) Create(owner domain.User, name, sourceURL, keyword string) (domain.Feed, error) {
	return s.CreateWithOperator(owner, name, sourceURL, domain.FilterContains, keyword)
}

func (s *SQLiteStore) CreateWithOperator(owner domain.User, name, sourceURL string, operator domain.FilterOperator, keyword string) (domain.Feed, error) {
	userID, err := s.ownerID(owner)
	if err != nil {
		return domain.Feed{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Feed{}, fmt.Errorf("begin feed creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	base := domain.Slugify(name)
	if base == "" {
		base = "feed"
	}
	slug, err := uniqueFeedValue(tx, "slug", base, userID, true)
	if err != nil {
		return domain.Feed{}, fmt.Errorf("choose feed slug: %w", err)
	}
	path, err := uniqueFeedValue(tx, "path", base, userID, false)
	if err != nil {
		return domain.Feed{}, fmt.Errorf("choose feed path: %w", err)
	}
	createdAt := time.Now().UTC()
	result, err := tx.Exec(`
		INSERT INTO feeds (user_id, name, slug, path, source_url, filter_operator, keyword, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, name, slug, path, sourceURL, operator, keyword, createdAt.Format(time.RFC3339Nano))
	if err != nil {
		return domain.Feed{}, fmt.Errorf("create feed: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Feed{}, fmt.Errorf("read created feed id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Feed{}, fmt.Errorf("commit feed creation: %w", err)
	}
	return domain.Feed{ID: id, TenantID: userID, Name: name, Slug: slug, Path: path, SourceURL: sourceURL, Operator: operator, Keyword: keyword, CreatedAt: createdAt}, nil
}

func (s *SQLiteStore) Delete(owner domain.User, path string) error {
	userID, err := s.ownerID(owner)
	if err != nil {
		return err
	}
	result, err := s.db.Exec("DELETE FROM feeds WHERE user_id = ? AND path = ?", userID, path)
	if err != nil {
		return fmt.Errorf("delete feed: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted feed: %w", err)
	}
	if count == 0 {
		return os.ErrNotExist
	}
	return nil
}

func (s *SQLiteStore) ImportLegacyFeeds(path string, owner domain.User) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy feed store: %w", err)
	}
	var records []legacyFeedRecord
	if len(data) > 0 {
		if err := json.Unmarshal(data, &records); err != nil {
			return fmt.Errorf("decode legacy feed store: %w", err)
		}
	}
	userID, err := s.ownerID(owner)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin legacy feed import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, record := range records {
		base := domain.Slugify(record.Slug)
		if base == "" {
			base = domain.Slugify(record.Name)
		}
		if base == "" {
			base = "feed"
		}
		createdAt := record.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		var exists bool
		if err := tx.QueryRow(`
			SELECT EXISTS(SELECT 1 FROM feeds
			WHERE user_id = ? AND name = ? AND source_url = ? AND keyword = ? AND created_at = ?)`,
			userID, record.Name, record.SourceURL, record.Keyword, createdAt.UTC().Format(time.RFC3339Nano)).Scan(&exists); err != nil {
			return fmt.Errorf("check legacy feed: %w", err)
		}
		if exists {
			continue
		}
		slug, err := uniqueFeedValue(tx, "slug", base, userID, true)
		if err != nil {
			return fmt.Errorf("choose legacy feed slug: %w", err)
		}
		feedPath, err := uniqueFeedValue(tx, "path", base, userID, false)
		if err != nil {
			return fmt.Errorf("choose legacy feed path: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO feeds (user_id, name, slug, path, source_url, filter_operator, keyword, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			userID, record.Name, slug, feedPath, record.SourceURL, domain.FilterContains, record.Keyword, createdAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("import legacy feed: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy feed import: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove legacy feed store: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ownerID(owner domain.User) (int64, error) {
	if owner.ID > 0 {
		var id int64
		if err := s.db.QueryRow("SELECT id FROM users WHERE id = ?", owner.ID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, ErrUserNotFound
			}
			return 0, fmt.Errorf("read feed owner: %w", err)
		}
		return id, nil
	}
	if owner.Username == "" {
		return 0, ErrUserNotFound
	}
	var id int64
	if err := s.db.QueryRow("SELECT id FROM users WHERE username = ?", owner.Username).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrUserNotFound
		}
		return 0, fmt.Errorf("read feed owner: %w", err)
	}
	return id, nil
}

func uniqueFeedValue(tx *sql.Tx, column, base string, userID int64, scoped bool) (string, error) {
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		query := "SELECT EXISTS(SELECT 1 FROM feeds WHERE " + column + " = ?)"
		args := []any{candidate}
		if scoped {
			query = "SELECT EXISTS(SELECT 1 FROM feeds WHERE user_id = ? AND " + column + " = ?)"
			args = []any{userID, candidate}
		}
		var exists bool
		if err := tx.QueryRow(query, args...).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFeed(row rowScanner) (domain.Feed, error) {
	var feed domain.Feed
	var createdAt string
	if err := row.Scan(&feed.ID, &feed.TenantID, &feed.Name, &feed.Slug, &feed.Path, &feed.SourceURL, &feed.Operator, &feed.Keyword, &createdAt); err != nil {
		return domain.Feed{}, err
	}
	var err error
	feed.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.Feed{}, fmt.Errorf("parse feed creation time: %w", err)
	}
	return feed, nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read sqlite migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, found := strings.Cut(entry.Name(), "_")
		if !found {
			return nil, fmt.Errorf("invalid sqlite migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("invalid sqlite migration version in %q", entry.Name())
		}
		if _, exists := seen[version]; exists {
			return nil, fmt.Errorf("duplicate sqlite migration version %d", version)
		}
		contents, err := migrationFS.ReadFile(filepath.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read sqlite migration %q: %w", entry.Name(), err)
		}
		seen[version] = struct{}{}
		migrations = append(migrations, migration{version: version, name: entry.Name(), sql: string(contents)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

func prepareDatabaseFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
