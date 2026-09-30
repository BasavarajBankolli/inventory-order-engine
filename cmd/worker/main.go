// Command worker runs the background jobs (see internal/app/worker.go):
// today, expiring overdue reservations and resolving stuck payments.
//
// It is a separate process from the API: it keeps working when nobody calls
// the API, it cannot slow down HTTP requests, and running several copies is
// safe because every job is idempotent.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"inventory-order-engine/internal/app"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/logging"
	"inventory-order-engine/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker stopped with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, cfg.LogLevel).With("service", "worker")
	slog.SetDefault(logger)

	// Cancelled on Ctrl+C or `docker stop`: the current job finishes its
	// in-progress transaction (or rolls it back) and the loop exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	jobs, err := app.NewWorkerJobs(cfg, pool)
	if err != nil {
		return err
	}

	worker.Run(ctx, logger, cfg.WorkerInterval, jobs...)
	return nil
}
