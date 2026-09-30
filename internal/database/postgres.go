// Package database owns the PostgreSQL connection pool and the migration
// runner. Business modules (users, products, orders, ...) receive the pool
// through their constructors; they never open connections themselves.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect creates a PostgreSQL connection pool and verifies it can reach the
// database.
//
// Why a pool? Opening a TCP connection + authenticating on every query is
// slow. A pool keeps a few connections open and lends them to goroutines.
// It is safe for concurrent use, so the whole app shares one *pgxpool.Pool.
func Connect(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Do not wrap the raw error text into logs verbatim at call sites:
		// the URL contains the password. ParseConfig errors do not echo it.
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// NewWithConfig connects lazily. Ping now so a wrong password or a
	// stopped database fails at startup, not on the first user request.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
