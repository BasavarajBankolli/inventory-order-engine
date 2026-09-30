package users_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/testutil"
	"inventory-order-engine/internal/users"
)

// Integration tests against real PostgreSQL (skipped without TEST_DATABASE_URL).

func newUser(email string) users.NewUser {
	return users.NewUser{Email: email, Name: "Test User", PasswordHash: "$2a$04$fakehashfakehashfakehash", Role: identity.RoleCustomer}
}

func TestRepository_CreateAndGet(t *testing.T) {
	repo := users.NewRepository(testutil.NewMigratedPool(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, newUser("Alice@Example.com"))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID == 0 || created.CreatedAt.IsZero() {
		t.Errorf("database-generated fields missing: %+v", created)
	}

	byID, err := repo.GetByID(ctx, created.ID)
	if err != nil || byID.Email != "Alice@Example.com" {
		t.Errorf("GetByID() = %+v, %v", byID, err)
	}

	// CITEXT: lookup ignores case.
	byEmail, err := repo.GetByEmail(ctx, "alice@example.COM")
	if err != nil || byEmail.ID != created.ID {
		t.Errorf("GetByEmail(different case) = %+v, %v; want user %d", byEmail, err, created.ID)
	}
}

func TestRepository_NotFound(t *testing.T) {
	repo := users.NewRepository(testutil.NewMigratedPool(t))
	ctx := context.Background()

	if _, err := repo.GetByID(ctx, 999999); !errors.Is(err, users.ErrNotFound) {
		t.Errorf("GetByID(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByEmail(ctx, "ghost@example.com"); !errors.Is(err, users.ErrNotFound) {
		t.Errorf("GetByEmail(missing) error = %v, want ErrNotFound", err)
	}
}

func TestRepository_DuplicateEmailDifferentCase(t *testing.T) {
	repo := users.NewRepository(testutil.NewMigratedPool(t))
	ctx := context.Background()

	if _, err := repo.Create(ctx, newUser("bob@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, newUser("BOB@EXAMPLE.COM")); !errors.Is(err, users.ErrEmailTaken) {
		t.Errorf("Create(duplicate) error = %v, want ErrEmailTaken", err)
	}
}

// Many goroutines register the SAME email at the same instant. A
// "SELECT first, then INSERT" design would let several of them through.
// Relying on the UNIQUE constraint lets exactly one win.
func TestRepository_ConcurrentRegistrationSameEmail(t *testing.T) {
	repo := users.NewRepository(testutil.NewMigratedPool(t))
	ctx := context.Background()

	const attempts = 20
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		taken     int
		other     []error
	)
	start := make(chan struct{}) // closed to release all goroutines at once

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.Create(ctx, newUser("race@example.com"))

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, users.ErrEmailTaken):
				taken++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 1 || taken != attempts-1 || len(other) > 0 {
		t.Errorf("successes=%d taken=%d other=%v; want 1, %d, none", successes, taken, other, attempts-1)
	}
}

func TestRepository_CheckConstraints(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	// Bypass the Go validation and hit the database directly: the CHECK
	// constraints are the last line of defence.
	bad := []struct{ name, sql string }{
		{"unknown role", `INSERT INTO users (email, name, password_hash, role) VALUES ('r@x.com', 'R', 'h', 'SUPERADMIN')`},
		{"blank name", `INSERT INTO users (email, name, password_hash) VALUES ('n@x.com', '   ', 'h')`},
		{"empty hash", `INSERT INTO users (email, name, password_hash) VALUES ('h@x.com', 'H', '')`},
		{"supplied id", `INSERT INTO users (id, email, name, password_hash) VALUES (5, 'i@x.com', 'I', 'h')`},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tt.sql); err == nil {
				t.Errorf("insert succeeded, want constraint violation: %s", tt.sql)
			}
		})
	}
}
