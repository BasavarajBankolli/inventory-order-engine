package app

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/events"
	"inventory-order-engine/internal/testutil"
)

// The worker's wiring: NewWorkerJobs builds jobs that really work against
// the database, and that return errors (instead of crashing the worker)
// when the database is down.

func workerConfig() config.Config {
	return config.Config{
		JWTSecret: "worker-test-secret-that-is-32-bytes-long", JWTTTL: time.Hour,
		ReservationTTL: time.Minute, PaymentTimeout: time.Second, PaymentReconcileAfter: time.Minute,
		MockPaymentOutcome: "SUCCESS", WorkerBatchSize: 10, OutboxMaxAttempts: 3,
	}
}

func jobByName(t *testing.T, cfg config.Config, pool *pgxpool.Pool, name string) func(context.Context) error {
	t.Helper()
	jobs, err := NewWorkerJobs(cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Name == name {
			return j.Run
		}
	}
	t.Fatalf("no job named %q", name)
	return nil
}

func TestWorkerJobs_PublishOutbox(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	if err := events.NewOutbox(pool).Add(ctx, events.OrderCreated, events.AggregateOrder, 1, map[string]int{"order_id": 1}); err != nil {
		t.Fatal(err)
	}

	if err := jobByName(t, workerConfig(), pool, "publish-outbox")(ctx); err != nil {
		t.Fatalf("publish-outbox error = %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM outbox_events`).Scan(&status); err != nil || status != "PROCESSED" {
		t.Errorf("event status = %q, %v; want PROCESSED", status, err)
	}
}

func TestWorkerJobs_ExpireReservationsOnEmptyDatabase(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	if err := jobByName(t, workerConfig(), pool, "expire-reservations")(context.Background()); err != nil {
		t.Errorf("expire-reservations on an empty database: %v", err)
	}
}

// Database down: jobs return an error. worker.Run logs it and tries again
// on the next tick (tested in internal/worker) - the process stays up.
func TestWorkerJobs_DatabaseDownReturnsErrors(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	for _, name := range []string{"expire-reservations", "publish-outbox"} {
		if err := jobByName(t, workerConfig(), pool, name)(context.Background()); err == nil {
			t.Errorf("%s with the database down: error = nil", name)
		}
	}
}
