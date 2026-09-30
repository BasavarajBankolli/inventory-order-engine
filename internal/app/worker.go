package app

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/worker"
)

// NewWorkerJobs builds the background jobs run by cmd/worker. It uses the
// same services as the API, so business rules exist in exactly one place.
func NewWorkerJobs(cfg config.Config, pool *pgxpool.Pool) ([]worker.Job, error) {
	s, err := newServices(cfg, pool, Options{})
	if err != nil {
		return nil, err
	}

	expireReservations := worker.Job{
		Name: "expire-reservations",
		Run: func(ctx context.Context) error {
			rep, err := s.orders.ExpireOverdue(ctx, cfg.WorkerBatchSize)
			// Log only when something happened, so an idle worker stays quiet.
			if rep.Total() > 0 || rep.Failed > 0 {
				slog.InfoContext(ctx, "expire-reservations run finished",
					"expired", rep.Expired, "confirmed", rep.Confirmed, "cancelled", rep.Cancelled,
					"skipped", rep.Skipped, "failed", rep.Failed)
			}
			return err
		},
	}

	return []worker.Job{expireReservations}, nil
}
