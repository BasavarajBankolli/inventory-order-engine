package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
)

// Publisher delivers an event to the outside world (Kafka, RabbitMQ, SNS,
// a webhook, ...). An error means "not delivered, try again later".
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// LogPublisher "publishes" by writing the event to the log as JSON. It
// stands in for a real message broker: swapping in a Kafka publisher would
// be one new type implementing Publisher.
type LogPublisher struct {
	// FailureRate (0.0-1.0) makes a share of publishes fail on purpose, so
	// retries and backoff can be watched in a demo. 0 in normal use.
	FailureRate float64
}

// Publish implements Publisher.
func (p LogPublisher) Publish(ctx context.Context, e Event) error {
	if p.FailureRate > 0 && rand.Float64() < p.FailureRate {
		return errors.New("simulated broker failure")
	}
	msg, err := json.Marshal(e)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "event published", "event_id", e.ID, "event_type", e.Type,
		"aggregate_id", e.AggregateID, "message", json.RawMessage(msg))
	return nil
}

// Processor publishes committed outbox events.
type Processor struct {
	pool           *pgxpool.Pool
	outbox         *Outbox
	publisher      Publisher
	maxAttempts    int
	publishTimeout time.Duration
}

// NewProcessor creates a Processor. After maxAttempts failed publishes an
// event is marked FAILED and no longer retried.
func NewProcessor(pool *pgxpool.Pool, outbox *Outbox, publisher Publisher, maxAttempts int) *Processor {
	return &Processor{pool: pool, outbox: outbox, publisher: publisher,
		maxAttempts: maxAttempts, publishTimeout: 5 * time.Second}
}

// Result counts what one ProcessBatch call did.
type Result struct {
	Published int
	Retrying  int // failed now, will be retried after backoff
	Dead      int // failed for the last time -> FAILED
}

// ProcessBatch publishes up to batchSize due events.
//
//	BEGIN
//	  claim due events (FOR UPDATE SKIP LOCKED, per-aggregate order)
//	  for each: publish -> PROCESSED, or record the failure + next attempt
//	COMMIT
//
// DELIVERY IS AT-LEAST-ONCE. If Publish succeeds but the process crashes
// before COMMIT, the row is still PENDING and will be published AGAIN.
// There is no way around this with an external broker, so consumers must
// be idempotent: they remember event IDs they already handled. (The
// opposite choice - mark first, publish later - risks losing events, which
// is worse.)
//
// Only outbox rows are locked here, never orders or inventory, so a slow
// broker cannot block checkout. Each publish has its own timeout.
func (p *Processor) ProcessBatch(ctx context.Context, batchSize int) (Result, error) {
	var res Result
	err := database.WithTx(ctx, p.pool, func(tx pgx.Tx) error {
		outbox := p.outbox.WithTx(tx)
		due, err := outbox.claimDue(ctx, batchSize)
		if err != nil {
			return err
		}

		// claimDue returns at most ONE event per aggregate (the oldest
		// pending one), so ordering within an order is safe here: a later
		// event of the same order is only claimed once this one is done.
		for _, e := range due {
			pubCtx, cancel := context.WithTimeout(ctx, p.publishTimeout)
			pubErr := p.publisher.Publish(pubCtx, e)
			cancel()

			if pubErr == nil {
				if err := outbox.markProcessed(ctx, e.ID); err != nil {
					return fmt.Errorf("mark event %d processed: %w", e.ID, err)
				}
				res.Published++
				continue
			}

			dead, err := outbox.markFailedAttempt(ctx, e, pubErr, p.maxAttempts)
			if err != nil {
				return fmt.Errorf("record failed attempt for event %d: %w", e.ID, err)
			}
			if dead {
				res.Dead++
				slog.ErrorContext(ctx, "event publishing gave up; marked FAILED (dead letter)",
					"event_id", e.ID, "event_type", e.Type, "attempts", e.Attempts+1, "error", pubErr)
			} else {
				res.Retrying++
				slog.WarnContext(ctx, "event publish failed; will retry",
					"event_id", e.ID, "event_type", e.Type, "attempt", e.Attempts+1,
					"retry_in", backoff(e.Attempts+1).String(), "error", pubErr)
			}
		}
		return nil
	})
	return res, err
}

// Drain calls ProcessBatch until nothing more is published (or maxRounds
// is reached). Because each batch holds at most one event per order, an
// order's 2nd event (e.g. InventoryReserved after OrderCreated) becomes due
// only after the 1st one committed; draining publishes it in the same run
// instead of waiting for the next worker tick.
func (p *Processor) Drain(ctx context.Context, batchSize, maxRounds int) (Result, error) {
	var total Result
	for i := 0; i < maxRounds; i++ {
		r, err := p.ProcessBatch(ctx, batchSize)
		total.Published += r.Published
		total.Retrying += r.Retrying
		total.Dead += r.Dead
		if err != nil || r.Published == 0 {
			return total, err
		}
	}
	return total, nil
}
