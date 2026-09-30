package events

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/testutil"
)

// recorder is a Publisher that remembers what it published, in order, and
// can be told to fail for specific events.
type recorder struct {
	mu        sync.Mutex
	published []Event
	failFor   func(e Event, attempt int) bool
	attempts  map[int64]int
}

func (r *recorder) Publish(_ context.Context, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempts == nil {
		r.attempts = map[int64]int{}
	}
	r.attempts[e.ID]++
	if r.failFor != nil && r.failFor(e, r.attempts[e.ID]) {
		return errors.New("broker down")
	}
	r.published = append(r.published, e)
	return nil
}

func (r *recorder) ids() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, len(r.published))
	for i, e := range r.published {
		out[i] = e.ID
	}
	return out
}

func add(t *testing.T, pool *pgxpool.Pool, typ string, aggregateID int64) int64 {
	t.Helper()
	if err := NewOutbox(pool).Add(context.Background(), typ, AggregateOrder, aggregateID, map[string]int64{"order_id": aggregateID}); err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = pool.QueryRow(context.Background(), `SELECT max(id) FROM outbox_events`).Scan(&id)
	return id
}

func row(t *testing.T, pool *pgxpool.Pool, id int64) (status string, attempts int, lastError string, due bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT status, attempts, COALESCE(last_error, ''), next_attempt_at <= now()
		FROM outbox_events WHERE id = $1`, id).Scan(&status, &attempts, &lastError, &due)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// makeDue pretends the backoff delay has passed.
func makeDue(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE outbox_events SET next_attempt_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}

func TestBackoff(t *testing.T) {
	want := map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 8: 256 * time.Second, 9: 5 * time.Minute, 50: 5 * time.Minute}
	for attempts, d := range want {
		if got := backoff(attempts); got != d {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, d)
		}
	}
}

// THE outbox guarantee: the event shares the fate of the transaction.
func TestAdd_IsPartOfTheTransaction(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	outbox := NewOutbox(pool)

	tx, _ := pool.Begin(ctx)
	if err := outbox.WithTx(tx).Add(ctx, OrderCreated, AggregateOrder, 1, map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	tx.Rollback(ctx)

	tx, _ = pool.Begin(ctx)
	if err := outbox.WithTx(tx).Add(ctx, OrderConfirmed, AggregateOrder, 2, map[string]int{"n": 2}); err != nil {
		t.Fatal(err)
	}
	tx.Commit(ctx)

	var types []string
	rows, _ := pool.Query(ctx, `SELECT event_type FROM outbox_events`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		types = append(types, s)
	}
	if fmt.Sprint(types) != "[OrderConfirmed]" {
		t.Errorf("events = %v, want only the committed one", types)
	}
}

func TestProcessBatch_PublishesAndMarksProcessed(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	a := add(t, pool, OrderCreated, 1)
	b := add(t, pool, OrderCreated, 2)

	pub := &recorder{}
	res, err := NewProcessor(pool, NewOutbox(pool), pub, 5).ProcessBatch(context.Background(), 10)
	if err != nil || res.Published != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if fmt.Sprint(pub.ids()) != fmt.Sprint([]int64{a, b}) {
		t.Errorf("published %v, want [%d %d] in id order", pub.ids(), a, b)
	}
	for _, id := range []int64{a, b} {
		if status, attempts, _, _ := row(t, pool, id); status != "PROCESSED" || attempts != 1 {
			t.Errorf("event %d: %s after %d attempts", id, status, attempts)
		}
	}

	// Running again publishes nothing: processed events are never re-sent.
	res, _ = NewProcessor(pool, NewOutbox(pool), pub, 5).ProcessBatch(context.Background(), 10)
	if res.Published != 0 {
		t.Errorf("second run published %d, want 0", res.Published)
	}
}

func TestProcessBatch_RetryWithBackoffThenDeadLetter(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	id := add(t, pool, OrderCreated, 1)
	pub := &recorder{failFor: func(Event, int) bool { return true }} // broker always down
	p := NewProcessor(pool, NewOutbox(pool), pub, 3)
	ctx := context.Background()

	res, _ := p.ProcessBatch(ctx, 10)
	status, attempts, lastErr, due := row(t, pool, id)
	if res.Retrying != 1 || status != "PENDING" || attempts != 1 || lastErr != "broker down" || due {
		t.Fatalf("after 1st failure: %+v %s attempts=%d err=%q due=%v", res, status, attempts, lastErr, due)
	}

	// Backoff: not retried immediately.
	if res, _ := p.ProcessBatch(ctx, 10); res != (Result{}) {
		t.Errorf("retried before the backoff elapsed: %+v", res)
	}

	makeDue(t, pool)
	p.ProcessBatch(ctx, 10) // attempt 2
	makeDue(t, pool)
	res, _ = p.ProcessBatch(ctx, 10) // attempt 3 = max
	if status, attempts, _, _ := row(t, pool, id); res.Dead != 1 || status != "FAILED" || attempts != 3 {
		t.Errorf("after max attempts: %+v %s attempts=%d, want FAILED after 3", res, status, attempts)
	}

	makeDue(t, pool)
	if res, _ := p.ProcessBatch(ctx, 10); res != (Result{}) {
		t.Errorf("a FAILED (dead) event was retried: %+v", res)
	}
}

func TestProcessBatch_RetrySucceedsLater(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	id := add(t, pool, OrderCreated, 1)
	pub := &recorder{failFor: func(_ Event, attempt int) bool { return attempt == 1 }} // first try fails
	p := NewProcessor(pool, NewOutbox(pool), pub, 5)

	p.ProcessBatch(context.Background(), 10)
	makeDue(t, pool)
	res, _ := p.ProcessBatch(context.Background(), 10)
	if status, attempts, lastErr, _ := row(t, pool, id); res.Published != 1 || status != "PROCESSED" || attempts != 2 || lastErr != "" {
		t.Errorf("%+v %s attempts=%d err=%q", res, status, attempts, lastErr)
	}
}

// Events of ONE order must reach consumers in order: if OrderCreated fails,
// OrderConfirmed of the same order must wait - but other orders continue.
func TestOrderingPerAggregate(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	created1 := add(t, pool, OrderCreated, 1)
	confirmed1 := add(t, pool, OrderConfirmed, 1)
	created2 := add(t, pool, OrderCreated, 2)

	pub := &recorder{failFor: func(e Event, attempt int) bool { return e.ID == created1 && attempt == 1 }}
	p := NewProcessor(pool, NewOutbox(pool), pub, 5)

	p.Drain(context.Background(), 10, 20)
	if fmt.Sprint(pub.ids()) != fmt.Sprint([]int64{created2}) {
		t.Fatalf("published %v, want only order 2's event (order 1 is blocked behind its failed first event)", pub.ids())
	}

	makeDue(t, pool)
	p.Drain(context.Background(), 10, 20)
	if fmt.Sprint(pub.ids()) != fmt.Sprint([]int64{created2, created1, confirmed1}) {
		t.Errorf("published %v, want order 1's events in order after the retry", pub.ids())
	}
}

// Five workers draining at the same time: every event published EXACTLY
// once (SKIP LOCKED), and each order's events still in order.
func TestConcurrentProcessors(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	for order := int64(1); order <= 20; order++ {
		add(t, pool, OrderCreated, order)
		add(t, pool, InventoryReserved, order)
		add(t, pool, OrderConfirmed, order)
	}

	pub := &recorder{}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := NewProcessor(pool, NewOutbox(pool), pub, 5)
			if _, err := p.Drain(context.Background(), 7, 50); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	seen := map[int64]int{}
	lastPerOrder := map[int64]int64{}
	for _, e := range pub.published {
		seen[e.ID]++
		if e.ID < lastPerOrder[e.AggregateID] {
			t.Errorf("order %d: event %d published after later event %d", e.AggregateID, e.ID, lastPerOrder[e.AggregateID])
		}
		lastPerOrder[e.AggregateID] = e.ID
	}
	if len(seen) != 60 {
		t.Errorf("published %d distinct events, want 60", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %d published %d times", id, n)
		}
	}
}
