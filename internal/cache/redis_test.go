package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// This test reproduces the outage we hit in Docker: Redis does not refuse
// the connection, it just never answers (a stopped container whose
// hostname lookup hangs). 10.255.255.1 is a non-routable address, so
// connecting to it hangs until the dial timeout.
//
// Without the breaker, each of these 20 calls would wait for the timeout
// (plus retries). With it, only the first one does; the rest fail instantly.
func TestRedisClient_FailsFastWhenRedisHangs(t *testing.T) {
	rdb, err := NewRedisClient("redis://10.255.255.1:6379/0")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	ctx := context.Background()

	start := time.Now()
	if err := rdb.Get(ctx, "k").Err(); err == nil {
		t.Fatal("expected an error from an unreachable Redis")
	}
	first := time.Since(start)

	start = time.Now()
	for i := 0; i < 20; i++ {
		err := rdb.Get(ctx, "k").Err()
		if !errors.Is(err, ErrRedisBypassed) {
			t.Fatalf("call %d: err = %v, want ErrRedisBypassed while the breaker is open", i, err)
		}
	}
	rest := time.Since(start)

	t.Logf("first call: %v, next 20 calls: %v", first, rest)
	if first > 3*time.Second {
		t.Errorf("first call took %v; dial timeouts/retries are too generous", first)
	}
	if rest > 50*time.Millisecond {
		t.Errorf("20 bypassed calls took %v; they must not touch the network", rest)
	}
}

func TestBreaker_OnlyConnectionErrorsOpenIt(t *testing.T) {
	b := newBreaker(time.Minute)

	b.record(nil)
	b.record(redis.Nil) // "key not found" is a normal answer
	if b.isOpen() {
		t.Fatal("breaker opened on a successful/Nil result")
	}

	b.record(errors.New("dial tcp: i/o timeout"))
	if !b.isOpen() {
		t.Fatal("breaker did not open on a connection error")
	}
}

func TestBreaker_ClosesAfterCooldown(t *testing.T) {
	b := newBreaker(20 * time.Millisecond)
	b.record(errors.New("connection refused"))
	if !b.isOpen() {
		t.Fatal("not open")
	}
	time.Sleep(30 * time.Millisecond)
	if b.isOpen() {
		t.Error("still open after the cooldown: the next call must be allowed as a probe")
	}
}
