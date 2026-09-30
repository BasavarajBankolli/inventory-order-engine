// Command api starts the HTTP API server.
//
// main() only starts things: load config -> create logger -> connect to
// PostgreSQL -> build the app (internal/app) -> serve -> shut down gracefully.
// No business logic lives here.
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
)

func main() {
	if err := run(); err != nil {
		slog.Error("api stopped with error", "error", err)
		os.Exit(1)
	}
}

// run contains the real startup logic. Returning an error (instead of
// calling os.Exit deep inside) lets every defer run and keeps main tiny.
func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, cfg.LogLevel)
	// Make it the default so slog.InfoContext(...) anywhere uses the same
	// JSON format. This is set once here and never changed afterwards.
	slog.SetDefault(logger)

	// ctx is cancelled when the process receives Ctrl+C (SIGINT) or
	// `docker stop` (SIGTERM). That is our signal to shut down.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("connected to postgres")

	handler, err := app.NewHandler(cfg, pool, logger, app.Options{})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
		// Timeouts protect the server from slow or malicious clients that
		// would otherwise hold connections (and goroutines) open forever.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// ListenAndServe blocks, so run it in a goroutine and report its
	// result through a channel.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		// The server failed to start (e.g. port already in use).
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Graceful shutdown: stop accepting new connections and wait for
	// in-flight requests to finish, but never longer than the timeout.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	logger.Info("http server stopped cleanly")
	return nil
}
