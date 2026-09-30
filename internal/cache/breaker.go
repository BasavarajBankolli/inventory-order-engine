package cache

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrRedisBypassed is returned while the circuit breaker is open.
var ErrRedisBypassed = errors.New("redis bypassed: recently unreachable")

// breaker is a tiny CIRCUIT BREAKER, installed as a go-redis hook so it
// protects every Redis call (cache and rate limiter alike).
//
// The problem it solves: when Redis is down, a call does not always fail
// instantly. In Docker, looking up the hostname of a stopped container can
// hang until a timeout. Without a breaker, EVERY request would pay that
// timeout (we measured 65-98 s per request before adding this), turning a
// cache outage into an API outage.
//
//	CLOSED    normal: calls go to Redis
//	   | a call fails with a connection/timeout error
//	   v
//	OPEN      for `cooldown`: calls fail IMMEDIATELY with ErrRedisBypassed,
//	   |      without touching the network -> callers fall back at once
//	   v      (cooldown over)
//	HALF-OPEN the next call is let through as a probe:
//	          success -> CLOSED, failure -> OPEN again
type breaker struct {
	cooldown  time.Duration
	openUntil atomic.Int64 // unix nanos; 0 or in the past = closed
}

func newBreaker(cooldown time.Duration) *breaker {
	return &breaker{cooldown: cooldown}
}

func (b *breaker) isOpen() bool {
	return time.Now().UnixNano() < b.openUntil.Load()
}

// record opens the breaker after an error that means "Redis is not
// reachable". Other results keep it closed:
//   - redis.Nil        "key not found" - a normal answer
//   - redis.Error      a reply FROM Redis (e.g. WRONGTYPE): Redis is up
func (b *breaker) record(err error) {
	if err == nil || errors.Is(err, redis.Nil) {
		return
	}
	var serverReply redis.Error
	if errors.As(err, &serverReply) {
		return
	}
	wasClosed := !b.isOpen()
	b.openUntil.Store(time.Now().Add(b.cooldown).UnixNano())
	if wasClosed {
		slog.Warn("redis unreachable; bypassing it", "for", b.cooldown.String(), "error", err)
	}
}

// DialHook implements redis.Hook (dialing is covered by ProcessHook).
func (b *breaker) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook wraps single commands (GET, SET, DEL, PING, ...).
func (b *breaker) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if b.isOpen() {
			cmd.SetErr(ErrRedisBypassed)
			return ErrRedisBypassed
		}
		err := next(ctx, cmd)
		b.record(err)
		return err
	}
}

// ProcessPipelineHook wraps pipelines/transactions (the rate limiter's
// INCR + EXPIRE).
func (b *breaker) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if b.isOpen() {
			for _, c := range cmds {
				c.SetErr(ErrRedisBypassed)
			}
			return ErrRedisBypassed
		}
		err := next(ctx, cmds)
		b.record(err)
		return err
	}
}
