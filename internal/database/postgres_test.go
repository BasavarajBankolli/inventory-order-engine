package database_test

import (
	"context"
	"testing"
	"time"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/testutil"
)

// Connect is what cmd/api, cmd/worker and cmd/migrate call at startup.
// It must fail FAST and CLEARLY when the database is wrong or missing.

func TestConnect_Success(t *testing.T) {
	pool, err := database.Connect(context.Background(), testutil.DatabaseURL(t), 3)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 3 {
		t.Errorf("MaxConns = %d, want 3", got)
	}
}

func TestConnect_BadURL(t *testing.T) {
	if _, err := database.Connect(context.Background(), "not a url ::", 1); err == nil {
		t.Error("Connect(bad url) error = nil")
	}
}

// Nothing listens on port 1: startup must report the problem instead of
// starting an API that fails on every request.
func TestConnect_DatabaseUnreachable(t *testing.T) {
	start := time.Now()
	_, err := database.Connect(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2", 1)
	if err == nil {
		t.Fatal("Connect(unreachable) error = nil")
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("took %v, want a quick failure", d)
	}
}
