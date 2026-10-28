package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/policy"
)

const (
	defaultTokenTTL       = 24 * time.Hour
	authCookieName        = "rss_studio_token"
	loginServiceTokenName = "login"
	browserSessionName    = "browser"
)

var (
	ErrInvalidAuthConfig         = errors.New("invalid authentication configuration")
	ErrInvalidCredentials        = errors.New("invalid credentials")
	ErrInvalidToken              = errors.New("invalid token")
	ErrRegistrationUnavailable   = errors.New("registration unavailable")
	ErrUserListingUnavailable    = errors.New("user listing unavailable")
	ErrServiceTokenUnavailable   = errors.New("service tokens unavailable")
	ErrBrowserSessionUnavailable = errors.New("browser sessions unavailable")
)

type AuthConfig struct {
	TokenTTL            time.Duration
	UserStore           UserStore
	ServiceTokenStore   ServiceTokenStore
	BrowserSessionStore BrowserSessionStore
}

type UserStore interface {
	Authenticate(username, password string) (domain.User, error)
}

type UserRegistrar interface {
	CreateUser(username, password string) (domain.User, error)
}

type UserLister interface {
	ListUsers() ([]domain.UserSummary, error)
}

type ServiceTokenStore interface {
	CreateServiceToken(owner domain.User, name string, scopes []string, expiresAt time.Time) (domain.ServiceToken, string, error)
	ListServiceTokens(owner domain.User) ([]domain.ServiceToken, error)
	RevokeServiceToken(owner domain.User, id int64) error
	AuthenticateServiceToken(raw string, now time.Time) (domain.ServiceToken, error)
}

type BrowserSessionStore interface {
	CreateBrowserSession(owner domain.User, expiresAt time.Time) (string, time.Time, error)
	AuthenticateBrowserSession(raw string, now time.Time) (domain.User, int64, error)
	RevokeBrowserSession(owner domain.User, raw string) error
}

type Authenticator struct {
	ttl             time.Duration
	now             func() time.Time
	userStore       UserStore
	serviceTokens   ServiceTokenStore
	browserSessions BrowserSessionStore
}

// RequestIdentity remains exported from httpapi for existing callers while
// its context and policy implementation live in the shared policy package.
type RequestIdentity = policy.Identity

func NewAuthenticator(config AuthConfig) (*Authenticator, error) {
	if config.UserStore == nil {
		return nil, fmt.Errorf("%w: user store is required", ErrInvalidAuthConfig)
	}
	if config.TokenTTL <= 0 {
		config.TokenTTL = defaultTokenTTL
	}
	serviceTokens := config.ServiceTokenStore
	if serviceTokens == nil {
		serviceTokens, _ = config.UserStore.(ServiceTokenStore)
	}
	if serviceTokens == nil {
		return nil, fmt.Errorf("%w: service token store is required", ErrInvalidAuthConfig)
	}
	browserSessions := config.BrowserSessionStore
	if browserSessions == nil {
		browserSessions, _ = config.UserStore.(BrowserSessionStore)
	}
	return &Authenticator{
		ttl:             config.TokenTTL,
		now:             time.Now,
		userStore:       config.UserStore,
		serviceTokens:   serviceTokens,
		browserSessions: browserSessions,
	}, nil
}

func (a *Authenticator) IssueToken(username, password string) (string, time.Time, error) {
	user, err := a.userStore.Authenticate(username, password)
	if err != nil {
		return "", time.Time{}, ErrInvalidCredentials
	}
	if a.serviceTokens == nil {
		return "", time.Time{}, ErrServiceTokenUnavailable
	}
	now := a.now().UTC()
	expires := now.Add(a.ttl)
	// Login intentionally preserves the former full-session behavior. Long-lived
	// integrations should use separately created, narrowly scoped tokens.
	scopes := domain.AllServiceTokenScopes()
	if !user.IsAdmin {
		scopes = []string{domain.ScopeFeedsRead, domain.ScopeFeedsPreview, domain.ScopeFeedsWrite, domain.ScopeFeedsDelete, domain.ScopeTokensManage}
	}
	_, token, err := a.serviceTokens.CreateServiceToken(user, loginServiceTokenName, scopes, expires)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func (a *Authenticator) IssueBrowserToken(username, password string) (string, time.Time, error) {
	user, err := a.userStore.Authenticate(username, password)
	if err != nil {
		return "", time.Time{}, ErrInvalidCredentials
	}
	if a.browserSessions == nil {
		return "", time.Time{}, ErrBrowserSessionUnavailable
	}
	expires := a.now().UTC().Add(a.ttl)
	token, expires, err := a.browserSessions.CreateBrowserSession(user, expires)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func (a *Authenticator) Register(username, password string) error {
	registrar, ok := a.userStore.(UserRegistrar)
	if !ok {
		return ErrRegistrationUnavailable
	}
	_, err := registrar.CreateUser(username, password)
	return err
}

func (a *Authenticator) ListUsers() ([]domain.UserSummary, error) {
	lister, ok := a.userStore.(UserLister)
	if !ok {
		return nil, ErrUserListingUnavailable
	}
	return lister.ListUsers()
}

func (a *Authenticator) CreateServiceToken(owner domain.User, name string, scopes []string, expiresAt time.Time) (domain.ServiceToken, string, error) {
	if a.serviceTokens == nil {
		return domain.ServiceToken{}, "", ErrServiceTokenUnavailable
	}
	return a.serviceTokens.CreateServiceToken(owner, name, scopes, expiresAt)
}

func (a *Authenticator) ListServiceTokens(owner domain.User) ([]domain.ServiceToken, error) {
	if a.serviceTokens == nil {
		return nil, ErrServiceTokenUnavailable
	}
	return a.serviceTokens.ListServiceTokens(owner)
}

func (a *Authenticator) RevokeServiceToken(owner domain.User, id int64) error {
	if a.serviceTokens == nil {
		return ErrServiceTokenUnavailable
	}
	return a.serviceTokens.RevokeServiceToken(owner, id)
}

func (a *Authenticator) RevokeBrowserSession(owner domain.User, raw string) error {
	if a.browserSessions == nil {
		return ErrBrowserSessionUnavailable
	}
	return a.browserSessions.RevokeBrowserSession(owner, raw)
}

func (a *Authenticator) ValidateRequest(request *http.Request) error {
	_, err := a.Subject(request)
	return err
}

func (a *Authenticator) UserFromRequest(request *http.Request) (domain.User, error) {
	identity, err := a.IdentityFromRequest(request)
	if err != nil {
		return domain.User{}, err
	}
	return identity.User, nil
}

func requestToken(request *http.Request) (string, error) {
	if authorization := request.Header.Get("Authorization"); authorization != "" {
		parts := strings.Fields(authorization)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", ErrInvalidToken
		}
		return parts[1], nil
	}
	cookie, err := request.Cookie(authCookieName)
	if err != nil {
		return "", ErrInvalidToken
	}
	return cookie.Value, nil
}

func withIdentity(ctx context.Context, identity RequestIdentity) context.Context {
	return policy.WithIdentity(ctx, identity)
}

func IdentityFromContext(ctx context.Context) (RequestIdentity, bool) {
	return policy.IdentityFromContext(ctx)
}

func userFromContext(ctx context.Context) (domain.User, bool) {
	identity, ok := IdentityFromContext(ctx)
	return identity.User, ok
}

func (a *Authenticator) Subject(request *http.Request) (string, error) {
	identity, err := a.IdentityFromRequest(request)
	if err != nil {
		return "", err
	}
	return identity.User.Username, nil
}

func (a *Authenticator) ValidateToken(token string) error {
	_, err := a.authenticateToken(token)
	return err
}

func (a *Authenticator) IdentityFromRequest(request *http.Request) (RequestIdentity, error) {
	if request.Header.Get("Authorization") != "" {
		token, err := requestToken(request)
		if err != nil {
			return RequestIdentity{}, err
		}
		identity, err := a.authenticateToken(token)
		if err != nil {
			return RequestIdentity{}, err
		}
		identity.CredentialHash = hashCredential(token)
		return identity, nil
	}
	return a.browserIdentityFromRequest(request)
}

func (a *Authenticator) BrowserIdentityFromRequest(request *http.Request) (RequestIdentity, error) {
	return a.browserIdentityFromRequest(request)
}

func (a *Authenticator) browserIdentityFromRequest(request *http.Request) (RequestIdentity, error) {
	if a.browserSessions == nil {
		return RequestIdentity{}, ErrInvalidToken
	}
	cookie, err := request.Cookie(authCookieName)
	if err != nil || cookie.Value == "" {
		return RequestIdentity{}, ErrInvalidToken
	}
	user, id, err := a.browserSessions.AuthenticateBrowserSession(cookie.Value, a.now().UTC())
	if err != nil || user.ID <= 0 || user.Username == "" || id <= 0 {
		return RequestIdentity{}, ErrInvalidToken
	}
	return RequestIdentity{
		User: user, TokenID: id, TokenName: browserSessionName,
		CredentialHash: hashCredential(cookie.Value),
	}, nil
}

func hashCredential(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (a *Authenticator) authenticateToken(token string) (RequestIdentity, error) {
	if a.serviceTokens == nil {
		return RequestIdentity{}, ErrInvalidToken
	}
	serviceToken, err := a.serviceTokens.AuthenticateServiceToken(token, a.now().UTC())
	if err != nil || serviceToken.UserID <= 0 || serviceToken.Username == "" {
		return RequestIdentity{}, ErrInvalidToken
	}
	user, err := a.userForSubject(serviceToken.Username)
	if err != nil {
		return RequestIdentity{}, ErrInvalidToken
	}
	if user.ID == 0 {
		user.ID = serviceToken.UserID
	}
	return RequestIdentity{
		User: user, TokenID: serviceToken.ID, TokenName: serviceToken.Name,
		Scopes: serviceToken.Scopes, ServiceToken: true,
	}, nil
}

func (a *Authenticator) userForSubject(subject string) (domain.User, error) {
	if lookup, ok := a.userStore.(interface {
		User(string) (domain.User, error)
	}); ok {
		user, err := lookup.User(subject)
		if err != nil {
			return domain.User{}, err
		}
		return user, nil
	}
	return domain.User{Username: subject}, nil
}
