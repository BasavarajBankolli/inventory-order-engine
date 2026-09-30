// Package worker runs background jobs on a timer, in their own process
// (cmd/worker), separate from the HTTP API.
//
// Why a separate process? Background work (expiring reservations, sending
// events) must happen even when nobody calls the API, must not slow down
// HTTP requests, and can be scaled or restarted independently.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Job is one piece of periodic work. Run must be SAFE TO REPEAT: it may run
// again after a crash, overlap with another worker instance, or find that
// the API already did the work. Every job in this project is written so
// that doing it twice has the same effect as doing it once (idempotent).
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Run executes every job once immediately, then again every interval, until
// ctx is cancelled (Ctrl+C / docker stop). Jobs run one after another in the
// same goroutine, so a slow job delays the next tick instead of piling up
// overlapping runs.
func Run(ctx context.Context, logger *slog.Logger, interval time.Duration, jobs ...Job) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("worker started", "interval", interval.String(), "jobs", len(jobs))
	for {
		for _, job := range jobs {
			if ctx.Err() != nil {
				break
			}
			runOne(ctx, logger, job)
		}

		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return
		case <-ticker.C:
		}
	}
}

// runOne runs a job and makes sure that neither an error nor a panic stops
// the worker: the job is simply tried again on the next tick.
func runOne(ctx context.Context, logger *slog.Logger, job Job) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			logger.ErrorContext(ctx, "job panicked", "job", job.Name,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()

	if err := job.Run(ctx); err != nil && ctx.Err() == nil {
		logger.ErrorContext(ctx, "job failed", "job", job.Name, "error", err,
			"duration_ms", time.Since(start).Milliseconds())
	}
}
