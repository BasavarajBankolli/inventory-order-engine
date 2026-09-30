// Command migrate applies pending database migrations and exits.
//
// It is a separate program (not part of the API startup) so that schema
// changes happen exactly once, as an explicit deployment step, before any
// new API version starts. docker-compose runs it automatically.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/logging"
	"inventory-order-engine/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := database.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		return err
	}

	logger.Info("migrations complete", "newly_applied", len(applied))
	return nil
}
