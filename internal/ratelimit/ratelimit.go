// Package ratelimit limits how many requests one client may make per
// minute, using counters in Redis.
//
// Why? To protect the API (and PostgreSQL behind it) from a single client
// sending far more requests than any human would: buggy retry loops,
// scrapers, password-guessing on /login.
//
// Why Redis and not a Go map? The API may run as several processes. A map
// would give each process its own counter (a client could get N x the
// limit); Redis is one shared counter for all of them.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter is a FIXED-WINDOW counter:
//
//	window = the current minute (12:03:00 - 12:03:59)
//	key    = rl:<subject>:<window start>        e.g. rl:user:42:1790770980
//	INCR key  -> 1, 2, 3, ... ; allowed while the count <= limit
//	the key expires with its window, so old counters clean themselves up
//
// Trade-off: at a window boundary a client can make up to 2 x limit
// requests in a short time (end of one minute + start of the next). A
// sliding window or token bucket smooths that out, at the cost of more
// complexity - a fixed window is plenty for this project.
type Limiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
	prefix string
	now    func() time.Time // replaceable in tests
}

// NewLimiter allows `limit` requests per `window` per subject. prefix keeps
// keys apart in a shared Redis (tests use a random one; production "").
func NewLimiter(rdb *redis.Client, limit int, window time.Duration, prefix string) *Limiter {
	return &Limiter{rdb: rdb, limit: limit, window: window, prefix: prefix, now: time.Now}
}

// Decision is the result of one Allow call.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration // until the current window ends
}

// Allow counts one request for subject (e.g. "user:42" or "ip:10.0.0.7")
// and says whether it is within the limit.
func (l *Limiter) Allow(ctx context.Context, subject string) (Decision, error) {
	now := l.now()
	windowStart := now.Truncate(l.window)
	key := fmt.Sprintf("%srl:%s:%d", l.prefix, subject, windowStart.Unix())

	// INCR and EXPIRE are sent together in one MULTI/EXEC transaction, so a
	// counter can never be created without its expiry. EXPIRE ... NX only
	// sets the TTL the first time (when the key is new).
	pipe := l.rdb.TxPipeline()
	count := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, l.window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return Decision{}, fmt.Errorf("rate limit counter: %w", err)
	}

	n := int(count.Val())
	return Decision{
		Allowed:    n <= l.limit,
		Limit:      l.limit,
		Remaining:  max(0, l.limit-n),
		RetryAfter: windowStart.Add(l.window).Sub(now),
	}, nil
}
