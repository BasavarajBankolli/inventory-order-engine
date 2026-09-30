// Package testutil contains helpers shared by tests. It is only imported
// from *_test.go files, never from production code.
package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseURL returns TEST_DATABASE_URL, or skips the test if it is not set.
//
// This lets `go test ./...` always work: unit tests run everywhere, while
// integration tests run only when a test database is available.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

// NewPool connects to the test database and closes the pool when the test
// ends. An optional searchPath isolates the test inside its own schema.
func NewPool(t *testing.T, searchPath string) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(DatabaseURL(t))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if searchPath != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = searchPath
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
