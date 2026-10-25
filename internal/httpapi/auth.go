package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"rss-studio/internal/domain"
)

const (
	defaultTokenTTL = 24 * time.Hour
	issuer          = "rss-studio"
	authCookieName  = "rss_studio_token"
)

var (
	ErrInvalidAuthConfig       = errors.New("invalid authentication configuration")
	ErrInvalidCredentials      = errors.New("invalid credentials")
	ErrInvalidToken            = errors.New("invalid token")
	ErrRegistrationUnavailable = errors.New("registration unavailable")
)

type AuthConfig struct {
	JWTSecret string
	TokenTTL  time.Duration
	UserStore UserStore
}

type UserStore interface {
	Authenticate(username, password string) (domain.User, error)
}

type UserRegistrar interface {
	CreateUser(username, password string) (domain.User, error)
}

type Authenticator struct {
	secret    []byte
	ttl       time.Duration
	now       func() time.Time
	userStore UserStore
}

type userContextKey struct{}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type jwtClaims struct {
	Subject string `json:"sub"`
	Issuer  string `json:"iss"`
	Issued  int64  `json:"iat"`
	Expires int64  `json:"exp"`
}

func NewAuthenticator(config AuthConfig) (*Authenticator, error) {
	if config.UserStore == nil || config.JWTSecret == "" {
		return nil, fmt.Errorf("%w: user store and JWT secret are required", ErrInvalidAuthConfig)
	}
	if len([]byte(config.JWTSecret)) < 32 {
		return nil, fmt.Errorf("%w: JWT secret must be at least 32 bytes", ErrInvalidAuthConfig)
	}
	if config.TokenTTL <= 0 {
		config.TokenTTL = defaultTokenTTL
	}
	return &Authenticator{
		secret:    []byte(config.JWTSecret),
		ttl:       config.TokenTTL,
		now:       time.Now,
		userStore: config.UserStore,
	}, nil
}

func (a *Authenticator) IssueToken(username, password string) (string, time.Time, error) {
	user, err := a.userStore.Authenticate(username, password)
	if err != nil {
		return "", time.Time{}, ErrInvalidCredentials
	}
	subject := user.Username

	now := a.now().UTC().Truncate(time.Second)
	expires := now.Add(a.ttl)
	header, err := json.Marshal(jwtHeader{Algorithm: "HS256", Type: "JWT"})
	if err != nil {
		return "", time.Time{}, err
	}
	claims, err := json.Marshal(jwtClaims{
		Subject: subject,
		Issuer:  issuer,
		Issued:  now.Unix(),
		Expires: expires.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}

	signingInput := encodeSegment(header) + "." + encodeSegment(claims)
	return signingInput + "." + encodeSegment(a.sign([]byte(signingInput))), expires, nil
}

func (a *Authenticator) Register(username, password string) error {
	registrar, ok := a.userStore.(UserRegistrar)
	if !ok {
		return ErrRegistrationUnavailable
	}
	_, err := registrar.CreateUser(username, password)
	return err
}

func (a *Authenticator) ValidateRequest(request *http.Request) error {
	_, err := a.Subject(request)
	return err
}

func (a *Authenticator) UserFromRequest(request *http.Request) (domain.User, error) {
	subject, err := a.Subject(request)
	if err != nil {
		return domain.User{}, err
	}
	if lookup, ok := a.userStore.(interface {
		User(string) (domain.User, error)
	}); ok {
		user, err := lookup.User(subject)
		if err != nil {
			return domain.User{}, ErrInvalidToken
		}
		return user, nil
	}
	return domain.User{Username: subject}, nil
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

func withUser(ctx context.Context, user domain.User) context.Context {
	return context.WithValue(ctx, userContextKey{}, user)
}

func userFromContext(ctx context.Context) (domain.User, bool) {
	user, ok := ctx.Value(userContextKey{}).(domain.User)
	return user, ok
}

func (a *Authenticator) Subject(request *http.Request) (string, error) {
	token, err := requestToken(request)
	if err != nil {
		return "", err
	}
	return a.TokenSubject(token)
}

func (a *Authenticator) ValidateToken(token string) error {
	_, err := a.TokenSubject(token)
	return err
}

func (a *Authenticator) TokenSubject(token string) (string, error) {
	if len(token) > 4096 {
		return "", ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", ErrInvalidToken
	}

	var header jwtHeader
	headerBytes, err := decodeSegment(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "HS256" || header.Type != "JWT" {
		return "", ErrInvalidToken
	}

	signature, err := decodeSegment(parts[2])
	if err != nil || !hmac.Equal(signature, a.sign([]byte(parts[0]+"."+parts[1]))) {
		return "", ErrInvalidToken
	}

	claimsBytes, err := decodeSegment(parts[1])
	if err != nil {
		return "", ErrInvalidToken
	}
	var claims jwtClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil || claims.Subject == "" || claims.Issuer != issuer {
		return "", ErrInvalidToken
	}
	now := a.now().Unix()
	if claims.Issued <= 0 || claims.Expires <= claims.Issued || claims.Expires <= now {
		return "", ErrInvalidToken
	}
	return claims.Subject, nil
}

func (a *Authenticator) sign(input []byte) []byte {
	hash := hmac.New(sha256.New, a.secret)
	_, _ = hash.Write(input)
	return hash.Sum(nil)
}

func encodeSegment(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeSegment(segment string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(segment)
}
