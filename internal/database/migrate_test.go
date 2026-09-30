package database_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/testutil"
	"inventory-order-engine/migrations"
)

// Integration tests: they need a real PostgreSQL (TEST_DATABASE_URL).
//
// Each test gets its own empty schema (testutil.NewIsolatedPool), so tests
// start from a clean slate and never see each other's tables.

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	// to_regclass resolves the name using search_path and returns NULL if
	// no such table exists.
	err := pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return exists
}

func TestMigrate_AppliesInOrderAndIsIdempotent(t *testing.T) {
	pool := testutil.NewIsolatedPool(t)
	ctx := context.Background()

	fsys := fstest.MapFS{
		// Deliberately listed out of order: the runner must sort by name.
		"000002_add_column.sql": {Data: []byte("ALTER TABLE widgets ADD COLUMN color TEXT;")},
		"000001_create.sql":     {Data: []byte("CREATE TABLE widgets (id BIGINT PRIMARY KEY);")},
		"README.txt":            {Data: []byte("not a migration, must be ignored")},
	}

	applied, err := database.Migrate(ctx, pool, fsys)
	if err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	if fmt.Sprint(applied) != "[000001_create 000002_add_column]" {
		t.Fatalf("applied = %v, want both migrations in order", applied)
	}
	if !tableExists(t, pool, "widgets") {
		t.Fatal("widgets table was not created")
	}

	// Running again must do nothing: this is what makes it safe to run the
	// migrate container on every `docker compose up`.
	applied, err = database.Migrate(ctx, pool, fsys)
	if err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second run applied %v, want nothing", applied)
	}
}

func TestMigrate_FailedMigrationIsRolledBackAndNotRecorded(t *testing.T) {
	pool := testutil.NewIsolatedPool(t)
	ctx := context.Background()

	fsys := fstest.MapFS{
		"000001_ok.sql": {Data: []byte("CREATE TABLE good (id BIGINT);")},
		// First statement succeeds, second fails -> the whole file must be
		// rolled back, so the "half_done" table must NOT exist afterwards.
		"000002_broken.sql": {Data: []byte("CREATE TABLE half_done (id BIGINT); SELECT * FROM no_such_table;")},
	}

	applied, err := database.Migrate(ctx, pool, fsys)
	if err == nil {
		t.Fatal("Migrate() error = nil, want failure from broken migration")
	}
	if fmt.Sprint(applied) != "[000001_ok]" {
		t.Errorf("applied = %v, want only 000001_ok", applied)
	}
	if tableExists(t, pool, "half_done") {
		t.Error("half_done exists: failed migration was not rolled back")
	}

	var recorded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE version = '000002_broken'").Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Error("failed migration was recorded as applied")
	}

	// After "fixing" the file, a new run applies it.
	fsys["000002_broken.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE half_done (id BIGINT);")}
	applied, err = database.Migrate(ctx, pool, fsys)
	if err != nil {
		t.Fatalf("Migrate() after fix error = %v", err)
	}
	if fmt.Sprint(applied) != "[000002_broken]" {
		t.Errorf("applied = %v, want [000002_broken]", applied)
	}
}

func TestMigrate_ConcurrentRunnersApplyEachMigrationOnce(t *testing.T) {
	pool := testutil.NewIsolatedPool(t)
	ctx := context.Background()

	// This migration fails if it runs twice (CREATE TABLE without
	// IF NOT EXISTS), so any double-apply shows up as an error.
	fsys := fstest.MapFS{
		"000001_create.sql": {Data: []byte("CREATE TABLE once_only (id BIGINT);")},
	}

	const runners = 5
	var wg sync.WaitGroup
	results := make(chan []string, runners)
	errs := make(chan error, runners)

	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := database.Migrate(ctx, pool, fsys)
			if err != nil {
				errs <- err
				return
			}
			results <- applied
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Migrate() error = %v", err)
	}
	total := 0
	for applied := range results {
		total += len(applied)
	}
	if total != 1 {
		t.Errorf("migration applied %d times in total, want exactly 1", total)
	}
}

func TestMigrate_RealMigrationsApply(t *testing.T) {
	pool := testutil.NewIsolatedPool(t)

	// The project's real migration files must apply cleanly to an empty
	// schema. This catches SQL syntax errors before deployment.
	if _, err := database.Migrate(context.Background(), pool, migrations.FS); err != nil {
		t.Fatalf("Migrate(real migrations) error = %v", err)
	}
}
