// Package policy contains authentication-independent authorization decisions
// shared by HTTP and future non-HTTP adapters.
package policy

import (
	"context"
	"errors"

	"rss-studio/internal/domain"
)

var (
	ErrInsufficientScope = errors.New("insufficient scope")
	ErrBrowserOnly       = errors.New("browser authentication required")
)

// Identity identifies the authenticated tenant and credential used for an
// operation. Browser JWTs intentionally have implicit access to existing
// operations; service tokens must carry each required scope explicitly.
type Identity struct {
	User         domain.User
	TokenID      int64
	TokenName    string
	Scopes       []string
	ServiceToken bool
}

type identityContextKey struct{}

// WithIdentity stores an immutable copy of identity in ctx.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	identity.Scopes = append([]string(nil), identity.Scopes...)
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns request identity propagated by authentication
// middleware.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)
	if ok {
		identity.Scopes = append([]string(nil), identity.Scopes...)
	}
	return identity, ok
}

// Authorize applies one shared policy decision for API endpoints and future
// adapters. Browser JWTs retain existing access; service tokens do not inherit
// scopes they were not granted.
func Authorize(identity Identity, requiredScope string) error {
	if !identity.ServiceToken || requiredScope == "" {
		return nil
	}
	for _, scope := range identity.Scopes {
		if scope == requiredScope {
			return nil
		}
	}
	return ErrInsufficientScope
}

// RequireBrowser rejects service-token identities for account-management
// operations that must remain available only to browser JWTs.
func RequireBrowser(identity Identity) error {
	if identity.ServiceToken {
		return ErrBrowserOnly
	}
	return nil
}
