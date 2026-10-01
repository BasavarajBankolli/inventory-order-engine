// Command worker runs the background jobs (see internal/app/worker.go):
// today, expiring overdue reservations and resolving stuck payments.
//
// It is a separate process from the API: it keeps working when nobody calls
// the API, it cannot slow down HTTP requests, and running several copies is
// safe because every job is idempotent.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"inventory-order-engine/internal/app"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/logging"
	"inventory-order-engine/internal/metrics"
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

	m := metrics.New()
	jobs, err := app.NewWorkerJobs(cfg, pool, m)
	if err != nil {
		return err
	}

	// The worker has no API, but Prometheus still needs to scrape its
	// metrics (expirations, outbox publishing), and Docker needs a health
	// check. A tiny separate HTTP server provides both.
	if cfg.WorkerMetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", m.Handler())
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		srv := &http.Server{Addr: cfg.WorkerMetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			logger.Info("worker metrics listening", "addr", cfg.WorkerMetricsAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("worker metrics server failed", "error", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	worker.Run(ctx, logger, cfg.WorkerInterval, jobs...)
	return nil
}
