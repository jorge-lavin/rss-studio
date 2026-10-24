package httpapi

import (
	"testing"
	"time"
)

func TestLoginRateLimiterLimitsClientAndResetsWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	limiter := &loginRateLimiter{
		attempts: make(map[string]loginRateLimitEntry),
		now:      func() time.Time { return now },
	}

	for attempt := 1; attempt <= loginAttemptsPerWindow; attempt++ {
		if allowed, _ := limiter.allow("192.0.2.1"); !allowed {
			t.Fatalf("attempt %d was unexpectedly limited", attempt)
		}
	}
	allowed, retryAfter := limiter.allow("192.0.2.1")
	if allowed || retryAfter != loginRateLimitWindow {
		t.Fatalf("expected client to be limited for %s, allowed=%t", loginRateLimitWindow, allowed)
	}

	now = now.Add(loginRateLimitWindow)
	if allowed, _ := limiter.allow("192.0.2.1"); !allowed {
		t.Fatal("client remained limited after rate-limit window")
	}
}
