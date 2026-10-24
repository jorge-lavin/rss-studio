package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteStoreMigratesAndBootstrapsUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss-studio.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("expected schema version 1, got %d", version)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("rerunning migrations: %v", err)
	}
	if err := store.BootstrapUser("admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := store.BootstrapUser("admin", "new password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate("admin", "correct horse"); err != nil {
		t.Fatalf("original bootstrap password rejected: %v", err)
	}
	if _, err := store.Authenticate("admin", "new password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expected replacement password to be rejected, got %v", err)
	}
	if _, err := store.Authenticate("missing", "password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expected missing user to be rejected, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("expected database permissions 0600, got %04o", permissions)
	}

	second, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Authenticate("admin", "correct horse"); err != nil {
		t.Fatalf("persisted user rejected: %v", err)
	}
}
