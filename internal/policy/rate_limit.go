package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitClass describes the rough cost of an operation. Limits are simple
// request counters; they are not weighted credits.
type RateLimitClass string

const (
	RateLimitCheapRead  RateLimitClass = "cheap_read"
	RateLimitDelete     RateLimitClass = "delete"
	RateLimitPreview    RateLimitClass = "preview"
	RateLimitCreate     RateLimitClass = "create"
	RateLimitPublicFeed RateLimitClass = "public_feed"
)

const (
	CheapReadRateLimit  = 120
	DeleteRateLimit     = 10
	PreviewRateLimit    = 20
	CreateRateLimit     = 10
	SplitRateLimit      = 5
	PublicFeedRateLimit = 60
	RateLimitWindow     = time.Minute
)

const (
	OperationFeedsList    = "feeds.list"
	OperationFeedsPreview = "feeds.preview"
	OperationFeedsCreate  = "feeds.create"
	OperationFeedsSplit   = "feeds.split"
	OperationFeedsDelete  = "feeds.delete"
	OperationFeedsPublic  = "feeds.public"
	OperationTokensList   = "auth.tokens.list"
	OperationTokensCreate = "auth.tokens.create"
	OperationTokensRevoke = "auth.tokens.revoke"
)

// RateLimitSpec is the fixed-window policy for one operation.
type RateLimitSpec struct {
	Operation string
	Class     RateLimitClass
	Limit     int
	Window    time.Duration
}

// RateLimitSpecFor returns the default request limit for an operation.
func RateLimitSpecFor(operation string) (RateLimitSpec, bool) {
	spec := RateLimitSpec{Operation: operation, Window: RateLimitWindow}
	switch operation {
	case OperationFeedsList, OperationTokensList:
		spec.Class, spec.Limit = RateLimitCheapRead, CheapReadRateLimit
	case OperationFeedsDelete, OperationTokensRevoke:
		spec.Class, spec.Limit = RateLimitDelete, DeleteRateLimit
	case OperationFeedsPreview:
		spec.Class, spec.Limit = RateLimitPreview, PreviewRateLimit
	case OperationFeedsCreate, OperationTokensCreate:
		spec.Class, spec.Limit = RateLimitCreate, CreateRateLimit
	case OperationFeedsSplit:
		spec.Class, spec.Limit = RateLimitCreate, SplitRateLimit
	case OperationFeedsPublic:
		spec.Class, spec.Limit = RateLimitPublicFeed, PublicFeedRateLimit
	default:
		return RateLimitSpec{}, false
	}
	return spec, true
}

// RateLimitOperationForHTTP maps versioned API routes to stable operations.
// Other adapters, including the MCP handler, should use the same operation
// names when entering the shared policy boundary.
func RateLimitOperationForHTTP(method, path string) (string, bool) {
	switch {
	case method == http.MethodGet && path == "/api/v1/feeds":
		return OperationFeedsList, true
	case method == http.MethodPost && path == "/api/v1/feeds":
		return OperationFeedsCreate, true
	case method == http.MethodPost && path == "/api/v1/feeds/preview":
		return OperationFeedsPreview, true
	case method == http.MethodPost && path == "/api/v1/feeds/split":
		return OperationFeedsSplit, true
	case method == http.MethodDelete && strings.HasPrefix(path, "/api/v1/feeds/"):
		return OperationFeedsDelete, true
	case method == http.MethodGet && path == "/api/v1/auth/tokens":
		return OperationTokensList, true
	case method == http.MethodPost && path == "/api/v1/auth/tokens":
		return OperationTokensCreate, true
	case (method == http.MethodPost || method == http.MethodDelete) && strings.HasPrefix(path, "/api/v1/auth/tokens/"):
		return OperationTokensRevoke, true
	default:
		return "", false
	}
}

// RateLimitBucket identifies one independent counter, without retaining a
// bearer credential. Account and token buckets are checked atomically.
type RateLimitBucket struct {
	Key string
}

// RateLimitBuckets returns account- and credential-scoped buckets shared by
// every authenticated adapter. It never retains a bearer credential.
func RateLimitBuckets(identity Identity) []RateLimitBucket {
	accountKey := "account:" + strconv.FormatInt(identity.User.ID, 10)
	if identity.User.ID <= 0 {
		accountKey = "account:" + hashRateLimitKey(identity.User.Username)
	}
	buckets := []RateLimitBucket{{Key: accountKey}}
	if identity.CredentialHash != "" {
		buckets = append(buckets, RateLimitBucket{Key: "token:" + hashRateLimitKey(identity.CredentialHash)})
	}
	return buckets
}

// PublicFeedRateLimitBucket returns a bucket keyed by the hashed public feed
// path, without retaining the path in rate-limit storage.
func PublicFeedRateLimitBucket(feedPath string) RateLimitBucket {
	return RateLimitBucket{Key: "public-feed:" + hashRateLimitKey(feedPath)}
}

func hashRateLimitKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// RateLimitDecision contains headers and retry information for one request.
type RateLimitDecision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAfter time.Duration
}

// RateLimitStore persists fixed-window counters for one service instance.
type RateLimitStore interface {
	CheckRateLimits(operation string, limit int, window time.Duration, buckets []RateLimitBucket, now time.Time) (RateLimitDecision, error)
}
