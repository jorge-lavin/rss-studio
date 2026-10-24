package storage

import (
	"database/sql"
	"embed"
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
