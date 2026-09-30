package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
)

// Outbox writes and reads outbox_events rows.
type Outbox struct {
	db database.DBTX
}

// NewOutbox creates an Outbox on db (normally the pool).
func NewOutbox(db database.DBTX) *Outbox {
	return &Outbox{db: db}
}

// WithTx returns an Outbox whose writes join the caller's transaction.
//
// This is THE important method: business code always records events via
// outbox.WithTx(tx).Add(...), inside the same transaction as its change.
func (o *Outbox) WithTx(tx pgx.Tx) *Outbox {
	return &Outbox{db: tx}
}

// Add records an event. payload is any JSON-serialisable value.
func (o *Outbox) Add(ctx context.Context, eventType, aggregateType string, aggregateID int64, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", eventType, err)
	}
	if _, err := o.db.Exec(ctx, `
		INSERT INTO outbox_events (event_type, aggregate_type, aggregate_id, payload)
		VALUES ($1, $2, $3, $4)`, eventType, aggregateType, aggregateID, data); err != nil {
		return fmt.Errorf("insert outbox event %s: %w", eventType, err)
	}
	return nil
}

const eventColumns = `id, event_type, aggregate_type, aggregate_id, payload, created_at, status, attempts, COALESCE(last_error, '')`

func scanEvent(row pgx.CollectableRow) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Type, &e.AggregateType, &e.AggregateID, &e.Payload, &e.CreatedAt,
		&e.Status, &e.Attempts, &e.LastError)
	return e, err
}

// claimDue locks up to limit publishable events. Must run inside a
// transaction; the locks are held until it commits.
//
//	status = 'PENDING' AND next_attempt_at <= now()   due (retries wait for their backoff)
//	NOT EXISTS (older PENDING event, same aggregate)   per-order ORDER: OrderConfirmed is
//	                                                   never published before OrderCreated,
//	                                                   even if OrderCreated needed retries
//	FOR UPDATE SKIP LOCKED                             several workers can run this at once:
//	                                                   each takes DIFFERENT rows instead of
//	                                                   waiting for (or double-publishing) the
//	                                                   rows another worker is busy with
//
// With two workers, the NOT EXISTS subquery still sees an older event that
// the other worker has locked (it is still PENDING until that worker
// commits), so ordering holds across workers too.
func (o *Outbox) claimDue(ctx context.Context, limit int) ([]Event, error) {
	rows, err := o.db.Query(ctx, `
		SELECT `+eventColumns+`
		FROM outbox_events e
		WHERE e.status = 'PENDING'
		  AND e.next_attempt_at <= now()
		  AND NOT EXISTS (
		        SELECT 1 FROM outbox_events older
		        WHERE older.aggregate_type = e.aggregate_type
		          AND older.aggregate_id   = e.aggregate_id
		          AND older.status         = 'PENDING'
		          AND older.id             < e.id)
		ORDER BY e.id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	events, err := pgx.CollectRows(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("scan outbox events: %w", err)
	}
	return events, nil
}

func (o *Outbox) markProcessed(ctx context.Context, id int64) error {
	_, err := o.db.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'PROCESSED', processed_at = now(), attempts = attempts + 1, last_error = NULL
		WHERE id = $1`, id)
	return err
}

// markFailedAttempt records a failed publish. The event either waits for
// its next attempt, or - after maxAttempts - becomes FAILED (dead letter).
func (o *Outbox) markFailedAttempt(ctx context.Context, e Event, publishErr error, maxAttempts int) (dead bool, err error) {
	attempts := e.Attempts + 1
	dead = attempts >= maxAttempts
	status := StatusPending
	if dead {
		status = StatusFailed
	}
	_, err = o.db.Exec(ctx, `
		UPDATE outbox_events
		SET attempts = $2, status = $3, last_error = $4,
		    next_attempt_at = now() + make_interval(secs => $5)
		WHERE id = $1`,
		e.ID, attempts, status, truncate(publishErr.Error(), 500), backoff(attempts).Seconds())
	return dead, err
}

// backoff is how long to wait before retry number `attempts`:
// 2s, 4s, 8s, 16s, ... capped at 5 minutes (EXPONENTIAL BACKOFF).
// Waiting longer after each failure gives a struggling broker room to
// recover instead of hammering it.
func backoff(attempts int) time.Duration {
	if attempts > 8 { // 2^9 s > 5 min anyway; also avoids overflow
		return 5 * time.Minute
	}
	return min(time.Duration(1<<attempts)*time.Second, 5*time.Minute)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
