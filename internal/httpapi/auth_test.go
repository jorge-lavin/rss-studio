package httpapi

import (
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/storage"
)

func testAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.CreateUser("admin", "password"); err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(AuthConfig{
		JWTSecret: strings.Repeat("x", 32),
		TokenTTL:  time.Hour,
		UserStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestNewAuthenticatorRequiresStrongConfiguration(t *testing.T) {
	for name, config := range map[string]AuthConfig{
		"missing user store": {JWTSecret: strings.Repeat("x", 32)},
		"missing secret":     {UserStore: authStoreStub{}},
		"short secret":       {UserStore: authStoreStub{}, JWTSecret: "too-short"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAuthenticator(config); !errors.Is(err, ErrInvalidAuthConfig) {
				t.Fatalf("expected invalid config, got %v", err)
			}
		})
	}
}

type authStoreStub struct{}

func (authStoreStub) Authenticate(username, password string) (domain.User, error) {
	return domain.User{Username: username}, nil
}

func TestAuthenticatorIssuesAndValidatesToken(t *testing.T) {
	auth := testAuthenticator(t)
	auth.now = func() time.Time { return time.Unix(1000, 0) }
	token, expires, err := auth.IssueToken("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	if expires.Unix() != 4600 {
		t.Fatalf("unexpected expiry: %s", expires)
	}
	if err := auth.ValidateToken(token); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	if err := auth.ValidateRequest(request); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	subject, err := auth.Subject(request)
	if err != nil || subject != "admin" {
		t.Fatalf("unexpected authenticated subject: %q, %v", subject, err)
	}
}

func TestAuthenticatorUsesPersistentUserStore(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateUser("admin", "database-password"); err != nil {
		t.Fatal(err)
	}

	auth, err := NewAuthenticator(AuthConfig{
		JWTSecret: strings.Repeat("x", 32),
		UserStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return time.Unix(1000, 0) }
	token, _, err := auth.IssueToken("admin", "database-password")
	if err != nil {
		t.Fatalf("persistent credentials rejected: %v", err)
	}
	if err := auth.ValidateToken(token); err != nil {
		t.Fatalf("persistent user token rejected: %v", err)
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	user, err := auth.UserFromRequest(request)
	if err != nil {
		t.Fatalf("persistent user identity rejected: %v", err)
	}
	if user.ID == 0 || user.Username != "admin" {
		t.Fatalf("unexpected persistent user identity: %#v", user)
	}
}

func TestAuthenticatorRegistersUsersInPersistentStore(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	auth, err := NewAuthenticator(AuthConfig{JWTSecret: strings.Repeat("x", 32), UserStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Register("new-user", "new-password"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	if _, _, err := auth.IssueToken("new-user", "new-password"); err != nil {
		t.Fatalf("registered credentials rejected: %v", err)
	}
}

func TestAuthenticatorRejectsInvalidTokens(t *testing.T) {
	auth := testAuthenticator(t)
	auth.now = func() time.Time { return time.Unix(1000, 0) }
	token, _, err := auth.IssueToken("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]string{
		"wrong credentials": "",
		"tampered":          token[:len(token)-1] + "x",
		"wrong scheme":      "Basic " + token,
		"missing token":     "Bearer",
	} {
		t.Run(name, func(t *testing.T) {
			if name == "wrong credentials" {
				if _, _, err := auth.IssueToken("admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
					t.Fatalf("expected invalid credentials, got %v", err)
				}
				return
			}
			if strings.HasPrefix(candidate, "Bearer") || strings.HasPrefix(candidate, "Basic") {
				request := httptest.NewRequest("GET", "/", nil)
				request.Header.Set("Authorization", candidate)
				if err := auth.ValidateRequest(request); !errors.Is(err, ErrInvalidToken) {
					t.Fatalf("expected invalid token, got %v", err)
				}
				return
			}
			if err := auth.ValidateToken(candidate); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("expected invalid token, got %v", err)
			}
		})
	}

	auth.now = func() time.Time { return time.Unix(4600, 0) }
	if err := auth.ValidateToken(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
}
