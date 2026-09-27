package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter applies a token-bucket limit per key, such as a username or a
// source address.
type RateLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	entries map[string]*limiterEntry
}

type limiterEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewRateLimiter allows burst events immediately, then one per interval.
func NewRateLimiter(interval time.Duration, burst int) *RateLimiter {
	return &RateLimiter{limit: rate.Every(interval), burst: burst, entries: make(map[string]*limiterEntry)}
}

// Allow reports whether an event for key is allowed now.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	e, ok := r.entries[key]
	if !ok {
		// Opportunistically drop idle entries so the map stays small.
		if len(r.entries) > 10000 {
			for k, v := range r.entries {
				if now.Sub(v.seen) > time.Hour {
					delete(r.entries, k)
				}
			}
		}
		e = &limiterEntry{lim: rate.NewLimiter(r.limit, r.burst)}
		r.entries[key] = e
	}
	e.seen = now
	return e.lim.Allow()
}
