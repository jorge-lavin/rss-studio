package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"rss-studio/internal/policy"
)

const (
	loginAttemptsPerWindow = 5
	loginRateLimitWindow   = time.Minute
)

type loginRateLimitEntry struct {
	started time.Time
	count   int
}

type loginRateLimiter struct {
	mu          sync.Mutex
	attempts    map[string]loginRateLimitEntry
	lastCleanup time.Time
	now         func() time.Time
}

func newLoginRateLimiter() *loginRateLimiter {
	return newLoginRateLimiterWithClock(time.Now)
}

func newLoginRateLimiterWithClock(now func() time.Time) *loginRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &loginRateLimiter{
		attempts: make(map[string]loginRateLimitEntry),
		now:      now,
	}
}

func (l *loginRateLimiter) allow(client string) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lastCleanup.IsZero() || !now.Before(l.lastCleanup.Add(loginRateLimitWindow)) {
		for key, entry := range l.attempts {
			if !now.Before(entry.started.Add(loginRateLimitWindow)) {
				delete(l.attempts, key)
			}
		}
		l.lastCleanup = now
	}

	entry, exists := l.attempts[client]
	if !exists || !now.Before(entry.started.Add(loginRateLimitWindow)) {
		l.attempts[client] = loginRateLimitEntry{started: now, count: 1}
		return true, 0
	}
	if entry.count >= loginAttemptsPerWindow {
		return false, entry.started.Add(loginRateLimitWindow).Sub(now)
	}

	entry.count++
	l.attempts[client] = entry
	return true, 0
}

func requestClient(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}

type apiRateLimiter struct {
	store policy.RateLimitStore
	now   func() time.Time
}

func newAPIRateLimiter(store policy.RateLimitStore, now func() time.Time) *apiRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &apiRateLimiter{store: store, now: now}
}

func (l *apiRateLimiter) allow(identity policy.Identity, operation string) (policy.RateLimitDecision, error) {
	spec, ok := policy.RateLimitSpecFor(operation)
	if !ok || l == nil || l.store == nil {
		return policy.RateLimitDecision{Allowed: true}, nil
	}
	return l.store.CheckRateLimits(spec.Operation, spec.Limit, spec.Window, policy.RateLimitBuckets(identity), l.now().UTC())
}

func (l *apiRateLimiter) allowPublicFeed(feedPath string) (policy.RateLimitDecision, error) {
	spec, ok := policy.RateLimitSpecFor(policy.OperationFeedsPublic)
	if !ok || l == nil || l.store == nil {
		return policy.RateLimitDecision{}, errors.New("rate limit storage unavailable")
	}
	return l.store.CheckRateLimits(spec.Operation, spec.Limit, spec.Window, []policy.RateLimitBucket{
		policy.PublicFeedRateLimitBucket(feedPath),
	}, l.now().UTC())
}

func rateLimitSeconds(duration time.Duration) int {
	seconds := int((duration + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func setRateLimitHeaders(writer http.ResponseWriter, decision policy.RateLimitDecision) {
	if decision.Limit <= 0 {
		return
	}
	remaining := decision.Remaining
	if remaining < 0 {
		remaining = 0
	}
	reset := rateLimitSeconds(decision.ResetAfter)
	writer.Header().Set("RateLimit-Limit", strconv.Itoa(decision.Limit))
	writer.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
	writer.Header().Set("RateLimit-Reset", strconv.Itoa(reset))
	// Keep the widely deployed spelling available to older API clients too.
	writer.Header().Set("X-RateLimit-Limit", strconv.Itoa(decision.Limit))
	writer.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	writer.Header().Set("X-RateLimit-Reset", strconv.Itoa(reset))
}

func rateLimitError(writer http.ResponseWriter, request *http.Request, decision policy.RateLimitDecision) {
	setRateLimitHeaders(writer, decision)
	writer.Header().Set("Retry-After", strconv.Itoa(rateLimitSeconds(decision.ResetAfter)))
	writeAPIError(writer, request, http.StatusTooManyRequests, "rate_limited", "Rate limit exceeded. Try again later.")
}

func operationNameForRequest(request *http.Request) (string, bool) {
	if request == nil || !strings.HasPrefix(request.URL.Path, "/api/v1/") {
		return "", false
	}
	return policy.RateLimitOperationForHTTP(request.Method, request.URL.Path)
}
