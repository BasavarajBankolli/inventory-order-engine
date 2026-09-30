package database

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is an arbitrary number used as the key of a PostgreSQL
// advisory lock. Every process that runs migrations uses the same key, so
// only one of them can migrate at a time.
const migrationLockID int64 = 727_274_001

// Migrate applies every *.sql file in fsys that has not been applied yet,
// in file-name order, and returns the versions it applied.
//
// How it works:
//
//  1. Take an advisory lock so two processes starting at the same moment
//     (e.g. two API containers) cannot apply the same migration twice.
//  2. Make sure the schema_migrations bookkeeping table exists.
//  3. For each file not yet recorded in schema_migrations:
//     BEGIN; run the file; INSERT its version; COMMIT.
//
// PostgreSQL supports DDL (CREATE TABLE, ALTER TABLE, ...) inside
// transactions, so a migration that fails half-way is rolled back completely
// and is NOT recorded: fix the file and run again.
//
// Rules for migration files:
//   - Name them NNNNNN_description.sql (zero-padded so sorting works).
//   - Never edit a file that has already been applied anywhere. Add a new one.
//   - Statements that cannot run in a transaction (e.g. CREATE INDEX
//     CONCURRENTLY) are not supported by this simple runner.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) ([]string, error) {
	// Advisory locks belong to a single connection (session), so we hold one
	// connection for the whole run instead of letting the pool pick any.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context: even if ctx was cancelled we must unlock,
		// because the pooled connection stays open and would keep the lock.
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			slog.Error("release migration lock", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations table: %w", err)
	}

	applied, err := appliedVersions(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	files, err := fs.Glob(migrations, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migration files: %w", err)
	}
	sort.Strings(files)

	var newlyApplied []string
	for _, file := range files {
		version := strings.TrimSuffix(path.Base(file), ".sql")
		if applied[version] {
			continue
		}

		sqlBytes, err := fs.ReadFile(migrations, file)
		if err != nil {
			return newlyApplied, fmt.Errorf("read %s: %w", file, err)
		}

		if err := applyOne(ctx, conn.Conn(), version, string(sqlBytes)); err != nil {
			return newlyApplied, fmt.Errorf("apply migration %s: %w", version, err)
		}
		slog.InfoContext(ctx, "migration applied", "version", version)
		newlyApplied = append(newlyApplied, version)
	}

	return newlyApplied, nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("query applied migrations: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}

	applied := make(map[string]bool, len(versions))
	for _, v := range versions {
		applied[v] = true
	}
	return applied, nil
}

// applyOne runs one migration and records it, atomically.
func applyOne(ctx context.Context, conn *pgx.Conn, version, sql string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	// Rollback after a successful Commit is a harmless no-op, so deferring it
	// is the standard way to guarantee cleanup on every error path.
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
