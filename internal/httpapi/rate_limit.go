package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
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
	return &loginRateLimiter{
		attempts: make(map[string]loginRateLimitEntry),
		now:      time.Now,
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
