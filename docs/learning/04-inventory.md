# 04 — Inventory, Transactions & Row Locking

## What problem does this solve?

We need to know how many units of each product can be sold, and that number must stay
**correct even when many requests change it at the same moment**. This stage introduces the
three tools the rest of the project depends on:

1. **Transactions**: several SQL statements that succeed or fail together.
2. **Pessimistic locking** (`SELECT ... FOR UPDATE`): make concurrent writers queue up.
3. **Optimistic locking** (a `version` column): detect that someone else changed the data since you looked at it.

## Why do we need it?

Picture two warehouse workers restocking the same product at the same time, without any locking:

```text
stock = 10
A: SELECT available  -> 10
B: SELECT available  -> 10
A: UPDATE available = 10 + 5  -> 15
B: UPDATE available = 10 + 3  -> 13     ❌ A's +5 is lost forever
```

This is the **lost update** problem. There are no errors and no crash, and the number is simply
wrong. With orders (Stage 5+), the same bug means **selling stock you don't have**.

## How does our implementation work?

```text
inventory.Handler     PATCH body -> UpdateInput                      (handler.go)
inventory.Service     transaction: lock -> check -> apply -> save    (service.go)
Inventory methods     Adjust / SetAvailable: pure business rules      (inventory.go)
inventory.Repository  Get, GetForUpdate, Save, CreateForProduct      (repository.go)
database.WithTx       BEGIN / COMMIT / ROLLBACK helper               (database/tx.go)
```

### Two kinds of update

| Request | Meaning | When to use it | Concurrency protection |
|---|---|---|---|
| `{"adjustment": 25}` | Add 25 (or remove, if negative) | Restock, damaged goods | Row lock. Always safe, because "+25" means the same thing whatever the current value is |
| `{"available_quantity": 100, "version": 3}` | Set to exactly 100 | Result of a physical stock count | Row lock **plus** a version check. "Set to 100" was decided from what you *saw*, so it's only valid if nothing changed since |

### The transaction (`inventory.Service.Update`)

```go
database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
    repo := s.repo.WithTx(tx)                 // every query below runs in tx
    inv, err := repo.GetForUpdate(ctx, id)    // SELECT ... FOR UPDATE: lock the row
    if in.Version != nil && *in.Version != inv.Version {
        return ErrVersionConflict             // client's view is stale -> ROLLBACK
    }
    err = inv.Adjust(*in.Adjustment)          // business rule in plain Go
    saved, err = repo.Save(ctx, inv)          // UPDATE ... version = version + 1
    return nil                                // -> COMMIT, lock released
})
```

Running the earlier race again, with the lock in place:

```text
stock = 10
A: BEGIN; SELECT ... FOR UPDATE  -> 10 (A holds the lock)
B: BEGIN; SELECT ... FOR UPDATE  -> ⏳ waits for A's lock
A: UPDATE available = 15; COMMIT (lock released)
B:                                -> 15 (B wakes up and sees A's committed value)
B: UPDATE available = 18; COMMIT                                   ✅ nothing lost
```

The writers now form a queue on that one row. Rows of other products are unaffected, so there
is no global slowdown.

### Why the rules live in Go methods

`Inventory.Adjust` and `Inventory.SetAvailable` don't know about SQL or HTTP. They take numbers
and either change them or return `ErrInsufficientStock` / `ErrQuantityTooLarge`. That makes the
core invariant readable in about 10 lines, and `inventory_test.go` checks it in microseconds.
The service's only job is to call them **while holding the lock**.

### Repositories and transactions: `DBTX` + `WithTx`

A repository stores a `database.DBTX`, an interface that both `*pgxpool.Pool` and `pgx.Tx`
satisfy. `repo.WithTx(tx)` returns a copy that runs its SQL inside `tx`. The **service decides
where a transaction begins and ends**, because only it knows which steps belong together.

### Every product has exactly one inventory row

- `products.Service.Create` now inserts the product **and** its empty inventory row in one transaction. If the second INSERT failed, the first would be rolled back too.
- Migration `000004` backfilled rows for products that already existed (`INSERT INTO inventory SELECT id FROM products`).
- Stage 5 relies on this: to place an order it locks the inventory row, and there is always a row to lock.

## What happens during a normal request?

`PATCH /api/v1/products/1/inventory {"adjustment": -3}` from an admin:

1. `RequireAuth` and `RequireRole(ADMIN)` let the request through.
2. `validateUpdate` checks there is exactly one of adjustment / available_quantity, the adjustment isn't 0, and it's within ±1,000,000.
3. `BEGIN`
4. `SELECT ... FROM inventory i JOIN products p ... WHERE product_id = 1 AND p.status <> 'ARCHIVED' FOR UPDATE OF i` locks the row and reads `available = 50, version = 2`.
5. `inv.Adjust(-3)` sets available to 47.
6. `UPDATE inventory SET available_quantity = 47, version = version + 1 WHERE product_id = 1 AND version = 2 RETURNING ...`
7. `COMMIT` releases the lock.
8. The service logs `"inventory updated" product_id=1 available_before=50 available_after=47 version=3 user_id=1 request_id=...`.
9. The response is `200 {"product_id":1,"available_quantity":47,"reserved_quantity":0,"version":3,...}`.

## What happens when it fails?

| Situation | Result | Database state |
|---|---|---|
| Adjustment would make stock negative | 409 `INSUFFICIENT_STOCK` | ROLLBACK, unchanged |
| `version` doesn't match (someone changed it) | 409 `VERSION_CONFLICT`. Client should GET again and retry | ROLLBACK, unchanged |
| `available_quantity` without `version` | 400: absolute updates must prove they're based on fresh data | nothing ran |
| Both fields, neither, or 0 | 400 `VALIDATION_ERROR` | nothing ran |
| `reserved_quantity` in the body | 400 unknown field. Only orders (Stage 6) may change it | nothing ran |
| Missing or archived product | 404 | ROLLBACK |
| Customer token / no token | 403 / 401 | nothing ran |
| A bug tries to store -1 | PostgreSQL rejects it with `CHECK (available_quantity >= 0)`, and the API returns 500 | ROLLBACK, unchanged |
| The process crashes mid-transaction | PostgreSQL rolls the open transaction back when the connection drops | unchanged |

## What database concepts are involved?

| Concept | Where | Why |
|---|---|---|
| **Transaction** (`BEGIN`/`COMMIT`/`ROLLBACK`) | `database.WithTx` | All-or-nothing groups of statements |
| **Row lock** `SELECT ... FOR UPDATE OF i` | `Repository.GetForUpdate` | Makes concurrent read-modify-write sequences wait in line. `OF i` locks only the inventory row, not the joined product |
| **Isolation level READ COMMITTED** (PostgreSQL's default) | every transaction | Each statement sees data committed before it started. After waiting for a lock, PostgreSQL re-reads the row, so B sees A's 15 |
| **Optimistic locking** (`version` column) | `Save ... WHERE version = $4`, plus the service check | Detects stale data *across requests*, where a lock can't be held (a human looks at the page, then submits minutes later) |
| **`FOREIGN KEY (product_id) REFERENCES products`** | migration 000004 | Inventory can only exist for real products, and those products can't be hard-deleted |
| **`UNIQUE (product_id)`** | migration 000004 | Exactly one row per product, and the index every lookup uses |
| **`CHECK (available_quantity >= 0)`** | migration 000004 | The last line of defence for "never negative" |
| **Data migration** (`INSERT ... SELECT`) | migration 000004 | Changes existing data, not just the schema |
| **Sequences aren't transactional** | ids | A failed INSERT still uses up an id. That's why the next product was id 5 when id 4 was never stored. Gaps are normal and harmless |

### Pessimistic vs optimistic locking

| | Pessimistic (`FOR UPDATE`) | Optimistic (`version`) |
|---|---|---|
| Idea | "Wait until I'm done" | "Fail if it changed since I looked" |
| Conflict result | The second writer **waits**, then proceeds | The second writer gets **409** and must retry |
| Scope | Inside one transaction (milliseconds) | Across requests (seconds or minutes) |
| Good for | Hot rows with many writers (stock during a sale) | Rare conflicts, human editing |

We use both. The lock protects each transaction, and the version protects the admin's decision.

## What concurrency issues exist?

- **Lost updates** are prevented by `FOR UPDATE`. `TestConcurrency_NoLostUpdates` sends 50 simultaneous `+1` requests and checks for exactly 50.
- **Negative stock under load** is prevented by the lock plus `Adjust` plus the CHECK constraint. `TestConcurrency_NeverNegative` starts with stock 5 and sends 20 simultaneous `-1` requests: exactly 5 succeed, 15 get `ErrInsufficientStock`, and stock ends at 0.
- **Stale absolute writes** are prevented by the version check. `TestConcurrency_OptimisticLocking` has 10 admins all submitting version 1: exactly 1 wins and 9 get a conflict.
- **Deadlocks** happen when two transactions each hold a lock the other needs (A locks row 1 then wants 2; B locks row 2 then wants 1). This stage only ever locks one row per transaction, so it can't deadlock. Stage 7 orders lock several rows, so they must always lock them in the same order (by product id).
- **Holding locks too long**: never do slow work (HTTP calls, payments) inside a transaction that holds row locks. Every other writer to that row waits. Stage 9 keeps the payment call outside the inventory transaction for exactly this reason.

## How can I reproduce/test it?

```powershell
# Unit tests for the rules
go test ./internal/inventory/ -run "Adjust|SetAvailable|ValidateUpdate" -v

# Concurrency and database tests (run them several times to trust them)
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=10 -run Concurrency ./internal/inventory/ -v
go test -count=1 ./tests/ -run Inventory -v
```

Manually, as an admin (see the README for getting `$admin`):

```powershell
$base = "http://localhost:8080/api/v1"
Invoke-RestMethod -Uri "$base/products/2/inventory" -Headers $admin
Invoke-RestMethod -Method Patch -Uri "$base/products/2/inventory" -Headers $admin -ContentType "application/json" -Body '{"adjustment": 50}'

# 20 parallel +1 requests; the result is exactly +20
$jobs = 1..20 | ForEach-Object { Start-Job -ScriptBlock { param($h) Invoke-RestMethod -Method Patch -Uri "http://localhost:8080/api/v1/products/2/inventory" -Headers $h -ContentType "application/json" -Body '{"adjustment": 1}' } -ArgumentList $admin }
$jobs | Wait-Job | Remove-Job
Invoke-RestMethod -Uri "$base/products/2/inventory" -Headers $admin
```

### Experiments to try

1. **Watch a lock block.** Open two psql windows (`docker compose exec postgres psql -U app -d inventory`).
   - In window 1, run: `BEGIN; SELECT * FROM inventory WHERE product_id = 1 FOR UPDATE;`
   - In window 2, run: `UPDATE inventory SET available_quantity = available_quantity + 1 WHERE product_id = 1;`. It hangs.
   - Type `COMMIT;` in window 1, and window 2 finishes instantly.
2. **Remove the safety nets one at a time.** Run `go test -count=3 -run NoLostUpdates ./internal/inventory/` after each step, then undo your changes.
   - Delete `FOR UPDATE OF i` from `GetForUpdate`. The test now fails with `ErrVersionConflict` errors. The second safety net (`WHERE version = $4` in `Save`) caught the race, so nothing was lost, but requests fail that should have succeeded.
   - Also, in `Save`, delete `AND i.version = $4` **and** the `inv.Version` argument below it (otherwise pgx complains `expected 3 arguments, got 4`). Now every request "succeeds", but the test reports something like `available = 6, version = 51; want 50`. All 50 requests returned 200, yet 44 restocks vanished without a single error. That's a real lost update.
3. **See the CHECK constraint save you.** Temporarily make `Adjust` skip its `newQty < 0` check, then send `{"adjustment": -1000}`. You get a 500 (PostgreSQL error `23514`, check violation) instead of stored negative stock. The logs show the real error with the request_id.
4. **Optimistic conflict by hand.** GET the inventory and note the version. PATCH `{"adjustment": 1}`. Now PATCH `{"available_quantity": 10, "version": <old version>}` and you get 409.
5. **Look at the locks.** While window 1 from experiment 1 holds its lock, run this in a third window: `SELECT locktype, relation::regclass, mode, granted FROM pg_locks WHERE relation = 'inventory'::regclass;`
