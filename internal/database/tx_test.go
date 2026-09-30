package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/testutil"
)

// WithTx must COMMIT on success and ROLLBACK on error or panic.

func TestWithTx(t *testing.T) {
	pool := testutil.NewIsolatedPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE notes (text TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM notes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Success: both inserts are committed.
	err := database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO notes VALUES ('a')`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO notes VALUES ('b')`)
		return err
	})
	if err != nil || count() != 2 {
		t.Fatalf("commit: err=%v rows=%d, want nil and 2", err, count())
	}

	// Error after a successful insert: the insert is rolled back too.
	boom := errors.New("business rule failed")
	err = database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO notes VALUES ('c')`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || count() != 2 {
		t.Fatalf("rollback on error: err=%v rows=%d, want boom and 2", err, count())
	}

	// Panic: rolled back, and the panic still reaches the caller.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		_ = database.WithTx(ctx, pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, `INSERT INTO notes VALUES ('d')`)
			panic("bug")
		})
	}()
	if count() != 2 {
		t.Fatalf("rollback on panic: rows=%d, want 2", count())
	}
}
