// Package testutil contains helpers shared by tests. It is only imported
// from *_test.go files, never from production code.
package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/requestid"
	"inventory-order-engine/migrations"
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
// ends. A non-empty searchPath makes unqualified table names resolve there.
func NewPool(t *testing.T, searchPath string) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(DatabaseURL(t))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if searchPath != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = searchPath
	}
	// Same cancellation behaviour as the production pool (database.Connect).
	database.CancelQueriesOnContextDone(cfg)

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

// NewIsolatedPool creates a brand-new, empty schema for this test and
// returns a pool whose search_path points at it. The schema is dropped when
// the test ends. Tests (and test packages running in parallel) therefore
// never see each other's tables or rows.
//
// search_path is "<schema>, public" so that extension types installed in
// public (like CITEXT) are still found.
func NewIsolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	schema := "test_" + requestid.New()[:12]

	admin := NewPool(t, "")
	ensureExtensions(t, admin)

	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})

	return NewPool(t, schema+", public")
}

// NewMigratedPool is NewIsolatedPool with all real migrations applied:
// an empty database with the production schema.
func NewMigratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := NewIsolatedPool(t)
	if _, err := database.Migrate(context.Background(), pool, migrations.FS); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return pool
}

// ensureExtensions installs extensions into the shared public schema once.
//
// Extensions are per-database, not per-schema. Without this, the first
// isolated schema would "own" citext and dropping it would break others.
// The advisory lock stops parallel test packages from racing on
// CREATE EXTENSION (which is not safe to run concurrently).
func ensureExtensions(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	// pg_advisory_xact_lock is released automatically at COMMIT/ROLLBACK.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(727274002)"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public"); err != nil {
		t.Fatalf("create extension citext: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
