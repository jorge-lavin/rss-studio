package policy_test

import (
	"context"
	"errors"
	"testing"

	"rss-studio/internal/domain"
	"rss-studio/internal/policy"
)

func TestAuthorizeTreatsBrowserJWTAsAlreadyAuthorized(t *testing.T) {
	identity := policy.Identity{User: domain.User{Username: "admin"}}
	if err := policy.Authorize(identity, domain.ScopeFeedsDelete); err != nil {
		t.Fatalf("browser JWT was denied: %v", err)
	}
}

func TestAuthorizeEnforcesEachServiceTokenScopeIndependently(t *testing.T) {
	for _, scope := range []string{
		domain.ScopeFeedsRead,
		domain.ScopeFeedsPreview,
		domain.ScopeFeedsWrite,
		domain.ScopeFeedsDelete,
	} {
		t.Run(scope, func(t *testing.T) {
			identity := policy.Identity{ServiceToken: true, Scopes: []string{scope}}
			if err := policy.Authorize(identity, scope); err != nil {
				t.Fatalf("granted scope was denied: %v", err)
			}
			if err := policy.Authorize(identity, "different-scope"); !errors.Is(err, policy.ErrInsufficientScope) {
				t.Fatalf("missing scope returned %v", err)
			}
		})
	}
}

func TestIdentityContextCopiesScopes(t *testing.T) {
	scopes := []string{domain.ScopeFeedsRead}
	ctx := policy.WithIdentity(context.Background(), policy.Identity{Scopes: scopes})
	scopes[0] = domain.ScopeFeedsDelete

	identity, ok := policy.IdentityFromContext(ctx)
	if !ok || len(identity.Scopes) != 1 || identity.Scopes[0] != domain.ScopeFeedsRead {
		t.Fatalf("identity was not safely propagated: %#v, %t", identity, ok)
	}
	identity.Scopes[0] = domain.ScopeFeedsDelete
	copy, _ := policy.IdentityFromContext(ctx)
	if copy.Scopes[0] != domain.ScopeFeedsRead {
		t.Fatalf("context identity was mutable: %#v", copy)
	}
}
