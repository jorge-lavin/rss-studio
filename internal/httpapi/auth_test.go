package httpapi

import (
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/storage"
)

func testAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	auth, err := NewAuthenticator(AuthConfig{
		Username:  "admin",
		Password:  "password",
		JWTSecret: strings.Repeat("x", 32),
		TokenTTL:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestNewAuthenticatorRequiresStrongConfiguration(t *testing.T) {
	for name, config := range map[string]AuthConfig{
		"missing username": {Password: "password", JWTSecret: strings.Repeat("x", 32)},
		"missing password": {Username: "admin", JWTSecret: strings.Repeat("x", 32)},
		"missing secret":   {Username: "admin", Password: "password"},
		"short secret":     {Username: "admin", Password: "password", JWTSecret: "too-short"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAuthenticator(config); !errors.Is(err, ErrInvalidAuthConfig) {
				t.Fatalf("expected invalid config, got %v", err)
			}
		})
	}
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
}

func TestAuthenticatorUsesPersistentUserStore(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rss-studio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.BootstrapUser("admin", "database-password"); err != nil {
		t.Fatal(err)
	}

	auth, err := NewAuthenticator(AuthConfig{
		Username:  "admin",
		Password:  "environment-password",
		JWTSecret: strings.Repeat("x", 32),
		UserStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return time.Unix(1000, 0) }
	if _, _, err := auth.IssueToken("admin", "environment-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected environment password to be ignored, got %v", err)
	}
	token, _, err := auth.IssueToken("admin", "database-password")
	if err != nil {
		t.Fatalf("persistent credentials rejected: %v", err)
	}
	if err := auth.ValidateToken(token); err != nil {
		t.Fatalf("persistent user token rejected: %v", err)
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
