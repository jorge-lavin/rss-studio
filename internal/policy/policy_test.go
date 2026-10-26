package policy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"rss-studio/internal/domain"
	"rss-studio/internal/policy"
)

func TestAuthorizeRequiresServiceToken(t *testing.T) {
	identity := policy.Identity{User: domain.User{Username: "admin"}}
	if err := policy.Authorize(identity, domain.ScopeFeedsDelete); !errors.Is(err, policy.ErrInsufficientScope) {
		t.Fatalf("unscoped identity returned %v", err)
	}
}

func TestPublicFeedRateLimitSpec(t *testing.T) {
	spec, ok := policy.RateLimitSpecFor(policy.OperationFeedsPublic)
	if !ok {
		t.Fatal("public feed operation was not recognized")
	}
	if spec.Operation != policy.OperationFeedsPublic || spec.Class != policy.RateLimitPublicFeed || spec.Limit != policy.PublicFeedRateLimit || spec.Window != time.Minute {
		t.Fatalf("unexpected public feed rate-limit spec: %#v", spec)
	}
}

func TestRateLimitSpecForPreservesAuthenticatedLimits(t *testing.T) {
	tests := []struct {
		operation string
		class     policy.RateLimitClass
		limit     int
	}{
		{policy.OperationFeedsList, policy.RateLimitCheapRead, policy.CheapReadRateLimit},
		{policy.OperationFeedsPreview, policy.RateLimitPreview, policy.PreviewRateLimit},
		{policy.OperationFeedsCreate, policy.RateLimitCreate, policy.CreateRateLimit},
		{policy.OperationFeedsSplit, policy.RateLimitCreate, policy.SplitRateLimit},
		{policy.OperationFeedsDelete, policy.RateLimitDelete, policy.DeleteRateLimit},
		{policy.OperationTokensList, policy.RateLimitCheapRead, policy.CheapReadRateLimit},
		{policy.OperationTokensCreate, policy.RateLimitCreate, policy.CreateRateLimit},
		{policy.OperationTokensRevoke, policy.RateLimitDelete, policy.DeleteRateLimit},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			spec, ok := policy.RateLimitSpecFor(test.operation)
			if !ok || spec.Class != test.class || spec.Limit != test.limit || spec.Window != time.Minute {
				t.Fatalf("authenticated rate-limit policy changed: %#v, recognized=%t", spec, ok)
			}
		})
	}
}

func TestPublicFeedRateLimitBucketHashesPathAndSeparatesFeeds(t *testing.T) {
	path := "/feeds/my-feed.xml"
	otherPath := "/feeds/other-feed.xml"
	sum := sha256.Sum256([]byte(path))
	want := "public-feed:" + hex.EncodeToString(sum[:])
	bucket := policy.PublicFeedRateLimitBucket(path)
	if bucket.Key != want {
		t.Fatalf("unexpected public feed bucket: got %q want %q", bucket.Key, want)
	}
	if strings.Contains(bucket.Key, path) {
		t.Fatalf("public feed bucket exposed raw path: %q", bucket.Key)
	}
	if bucket.Key == policy.PublicFeedRateLimitBucket(otherPath).Key {
		t.Fatal("different public feed paths shared a bucket")
	}
	for _, authenticatedBucket := range policy.RateLimitBuckets(policy.Identity{CredentialHash: "bearer-secret"}) {
		if bucket.Key == authenticatedBucket.Key {
			t.Fatal("public feed bucket overlapped authenticated bucket")
		}
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
