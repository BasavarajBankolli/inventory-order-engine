package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the set of query methods shared by *pgxpool.Pool and pgx.Tx.
//
// A repository that stores a DBTX (instead of a *pgxpool.Pool) can run its
// SQL either directly on the pool or inside a transaction, without knowing
// which. The SERVICE decides where a transaction starts and ends, because
// only the service knows which steps must succeed or fail together.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Compile-time checks: the build fails if either type stops matching DBTX.
var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (pgx.Tx)(nil)
)

// WithTx runs fn inside a database transaction.
//
//	BEGIN
//	  fn(tx)            every query made through tx is part of the transaction
//	COMMIT              if fn returned nil
//	ROLLBACK            if fn returned an error (or panicked)
//
// All-or-nothing: either every change made by fn becomes visible to other
// connections at once (COMMIT), or none of them ever does (ROLLBACK).
//
// The transaction uses PostgreSQL's default isolation level, READ COMMITTED:
// each statement sees data committed before that statement started. Code
// that must not lose concurrent updates locks rows explicitly with
// SELECT ... FOR UPDATE (see inventory.Repository.GetForUpdate).
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Runs on every exit path. After a successful Commit it is a no-op.
	// Also runs if fn panics, so a panic never leaves a transaction (and
	// its row locks) open.
	defer tx.Rollback(ctx)

	if err := fn(tx); err != nil {
		// If the context was cancelled (client gave up, shutdown), the
		// database error is just a symptom, e.g. "canceling statement due
		// to user request". Wrap it so callers can still recognise the
		// cause with errors.Is(err, context.Canceled).
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return fmt.Errorf("%w: %w", ctxErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
