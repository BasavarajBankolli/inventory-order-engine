// Package cache holds the Redis client setup and the product cache.
//
// Redis in this project is NEVER the source of truth. It only holds data
// that can be rebuilt from PostgreSQL at any time (cached products) or that
// is fine to lose (rate-limit counters). If Redis is down, the API keeps
// working - a little slower, and without rate limiting.
package cache

import (
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// breakerCooldown is how long Redis is skipped after it was unreachable.
const breakerCooldown = 5 * time.Second

// NewRedisClient creates a client from a URL like redis://redis:6379/0.
//
// Everything here is tuned to FAIL FAST, because Redis is optional:
//
//   - short timeouts: Redis normally answers in well under a millisecond;
//   - DialerRetries = 1: go-redis retries a failed dial 5 times by default,
//     which turned one unreachable Redis into ~11 s per call;
//   - ContextTimeoutEnabled: by default go-redis IGNORES context deadlines;
//   - a circuit breaker (breaker.go) that skips Redis entirely for a few
//     seconds after a connection failure.
//
// The client connects lazily, so this never fails because Redis is down.
func NewRedisClient(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	opts.DialTimeout = 500 * time.Millisecond
	opts.ReadTimeout = 300 * time.Millisecond
	opts.WriteTimeout = 300 * time.Millisecond
	opts.PoolTimeout = 500 * time.Millisecond
	opts.DialerRetries = 1
	opts.MaxRetries = 1
	opts.ContextTimeoutEnabled = true

	rdb := redis.NewClient(opts)
	rdb.AddHook(newBreaker(breakerCooldown))
	return rdb, nil
}
