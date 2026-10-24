package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTokenTTL = 24 * time.Hour
	issuer          = "rss-studio"
)

var (
	ErrInvalidAuthConfig  = errors.New("invalid authentication configuration")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidToken       = errors.New("invalid token")
)

type AuthConfig struct {
	Username  string
	Password  string
	JWTSecret string
	TokenTTL  time.Duration
}

type Authenticator struct {
	username string
	password string
	secret   []byte
	ttl      time.Duration
	now      func() time.Time
}

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
	if config.Username == "" || config.Password == "" || config.JWTSecret == "" {
		return nil, fmt.Errorf("%w: username, password, and JWT secret are required", ErrInvalidAuthConfig)
	}
	if len([]byte(config.JWTSecret)) < 32 {
		return nil, fmt.Errorf("%w: JWT secret must be at least 32 bytes", ErrInvalidAuthConfig)
	}
	if config.TokenTTL <= 0 {
		config.TokenTTL = defaultTokenTTL
	}
	return &Authenticator{
		username: config.Username,
		password: config.Password,
		secret:   []byte(config.JWTSecret),
		ttl:      config.TokenTTL,
		now:      time.Now,
	}, nil
}

func (a *Authenticator) IssueToken(username, password string) (string, time.Time, error) {
	if subtle.ConstantTimeCompare([]byte(username), []byte(a.username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(a.password)) != 1 {
		return "", time.Time{}, ErrInvalidCredentials
	}

	now := a.now().UTC().Truncate(time.Second)
	expires := now.Add(a.ttl)
	header, err := json.Marshal(jwtHeader{Algorithm: "HS256", Type: "JWT"})
	if err != nil {
		return "", time.Time{}, err
	}
	claims, err := json.Marshal(jwtClaims{
		Subject: a.username,
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

func (a *Authenticator) ValidateRequest(request *http.Request) error {
	parts := strings.Fields(request.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ErrInvalidToken
	}
	return a.ValidateToken(parts[1])
}

func (a *Authenticator) ValidateToken(token string) error {
	if len(token) > 4096 {
		return ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ErrInvalidToken
	}

	var header jwtHeader
	headerBytes, err := decodeSegment(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "HS256" || header.Type != "JWT" {
		return ErrInvalidToken
	}

	signature, err := decodeSegment(parts[2])
	if err != nil || !hmac.Equal(signature, a.sign([]byte(parts[0]+"."+parts[1]))) {
		return ErrInvalidToken
	}

	claimsBytes, err := decodeSegment(parts[1])
	if err != nil {
		return ErrInvalidToken
	}
	var claims jwtClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil || claims.Subject != a.username || claims.Issuer != issuer {
		return ErrInvalidToken
	}
	now := a.now().Unix()
	if claims.Issued <= 0 || claims.Expires <= claims.Issued || claims.Expires <= now {
		return ErrInvalidToken
	}
	return nil
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
