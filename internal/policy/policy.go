// Package policy contains authentication-independent authorization decisions
// shared by HTTP and non-HTTP adapters.
package policy

import (
	"context"
	"errors"

	"rss-studio/internal/domain"
)

var (
	ErrInsufficientScope = errors.New("insufficient scope")
)

// Identity identifies the authenticated tenant and credential used for an
// operation. Service tokens must carry each required scope explicitly.
type Identity struct {
	User      domain.User
	TokenID   int64
	TokenName string
	// CredentialHash binds request-scoped mutations without retaining the
	// bearer credential itself.
	CredentialHash string
	Scopes         []string
	ServiceToken   bool
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

// Authorize applies one shared policy decision for API endpoints and adapters.
// Unscoped identities cannot authorize operations.
func Authorize(identity Identity, requiredScope string) error {
	if requiredScope == "" {
		return nil
	}
	if !identity.ServiceToken {
		return ErrInsufficientScope
	}
	for _, scope := range identity.Scopes {
		if scope == requiredScope {
			return nil
		}
	}
	return ErrInsufficientScope
}
