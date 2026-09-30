# 07 — Transactions & Concurrency: Never Oversell

This stage adds almost no new features. Its job is to **prove** that the design from Stages 4–6
holds up when many requests arrive at the same instant, and to explain *why*.

## What problem does this solve?

```text
Stock = 1
Request A → buy 1         Request B → buy 1        (same millisecond)
```

The system must guarantee **1 success and 1 `OUT_OF_STOCK`**. Never 2 successes, never
`available = -1`. And it has to be the same with 100 or 10,000 requests.

## Why do we need it?

Web servers handle requests **in parallel**. Go gives each HTTP request its own goroutine, and
with a pool of 10 database connections, up to 10 transactions really do run at the same time
inside PostgreSQL. Code that is correct when requests arrive one by one can be badly wrong when
they overlap.

### The naive approach and its race

```go
stock := SELECT available FROM inventory WHERE product_id = 1   // A reads 1, B reads 1
if stock >= 1 {                                                 // both pass the check
    UPDATE inventory SET available = available - 1              // 1 → 0 → -1   ❌
    INSERT order                                                // two orders    ❌
}
```

Between A's check and A's update, B runs its check against the same old value. This is a
**check-then-act race** (also called TOCTOU: time of check to time of use). Even
`available = $computed_value` instead of `available - 1` just turns it into a lost update.

## How does our implementation work?

### 1. One transaction per business operation

```text
BEGIN
  ... every read and write for "place order" ...
COMMIT   (or ROLLBACK: nothing happened)
```

`database.WithTx` wraps each use case. A transaction gives **atomicity** (all or nothing) and
**isolation** (others don't see half-done work). But isolation at PostgreSQL's default level,
**READ COMMITTED**, does *not* stop the race above by itself: both transactions may still read
`available = 1`.

### 2. Pessimistic row locks: `SELECT ... FOR UPDATE`

```sql
SELECT ... FROM inventory WHERE product_id = 1 FOR UPDATE;   -- inventory.Repository.LockRow
```

Only one transaction at a time can hold this lock on a row. Everyone else **waits** at this line
until the holder commits or rolls back, then reads the **new** committed value:

```text
t0  A: BEGIN; SELECT ... FOR UPDATE   → available = 1   (A holds the lock)
t1  B: BEGIN; SELECT ... FOR UPDATE   → ⏳ waiting for A
t2  A: Reserve(1) → available 0, reserved 1; INSERT order, reservation
t3  A: COMMIT                          (lock released)
t4  B:                                → available = 0   (B wakes up and re-reads)
t5  B: Reserve(1) → ErrOutOfStock → ROLLBACK → 409 OUT_OF_STOCK
```

The check (`inv.Reserve`) and the write happen **while holding the lock**, so no other
transaction can slip in between. For one product, orders effectively form a queue; orders for
different products don't wait for each other.

### 3. A database safety net: CHECK constraints

```sql
CONSTRAINT inventory_available_nonnegative_check CHECK (available_quantity >= 0)
```

Even if a future bug skipped the lock or the check, PostgreSQL would refuse to store `-1` and
roll the transaction back. The application prevents overselling, and the database makes sure a
mistake can't be *stored*.

### 4. Deadlock prevention: one global lock order

A multi-item order locks several rows. If transaction A locks product 1 then 2, while B locks 2
then 1, each can end up holding one lock and waiting for the other. That's a **deadlock**.
PostgreSQL detects it after `deadlock_timeout` (1 s) and kills one of them with error `40P01`.

Our rule: **every transaction takes locks in the same order.**

```text
order row  →  reservation rows (ORDER BY product_id)  →  inventory rows (ascending product_id)
```

`ReserveForOrder` sorts the lines by product id before locking. With a single global order,
a waiting cycle can't form.

**Measured, not assumed.** `TestConcurrency_NoDeadlockWithOppositeItemOrder` sends 60 orders at
once, listing products as [A,B,C], [C,B,A] and [B,A,C]:

| Version | Successful orders | Deadlock errors | Time |
|---|---|---|---|
| With the sort (our code) | **60 / 60** | 0 | ~1.5 s |
| Sort removed (experiment) | 3–10 / 60 | 50–57 × `deadlock detected (SQLSTATE 40P01)` | 17–29 s |

### 5. Shared locks on products: `FOR SHARE`

Order creation reads the products with `SELECT ... FOR SHARE`:

| Lock | Can many hold it at once? | Blocks |
|---|---|---|
| `FOR SHARE` | Yes. 100 orders for one product don't wait for each other | `UPDATE` / `FOR UPDATE` on that row |
| `FOR UPDATE` | No, it's exclusive | Every other lock on the row |

So an admin who archives or re-prices a product in the middle of checkout **waits** until the
in-flight order transactions finish. The price and status an order saw are still true when it
commits. `TestConcurrency_ArchiveDuringOrders` checks this: every order either completes fully
before the archive, or is rejected after it. Nothing ends up half-done.

### Why this approach, and not another?

| Strategy | How | Why not (for us) |
|---|---|---|
| **Row lock `FOR UPDATE`** (chosen) | Lock, check in Go, write | Simple to read. The rule (`Reserve`) stays in Go and is unit-testable. Waits are short because the transaction contains no slow calls |
| Atomic conditional UPDATE | `UPDATE inventory SET available = available - $q WHERE product_id = $1 AND available >= $q` and check the rows affected | Also correct and very fast. But the business rule lives inside SQL, and a multi-step operation (reserve + reservation row + order) still needs a transaction |
| Optimistic locking (`version`) | Read, compute, `UPDATE ... WHERE version = $v`, retry on conflict | Under heavy contention (a flash sale on one product) most attempts conflict and retry: wasted work and extra latency. We use versions for admin edits instead (Stage 4) |
| `SERIALIZABLE` isolation | PostgreSQL detects dangerous interleavings and aborts one transaction (`40001`) | Correct, but every caller must retry on serialization failures, and it's harder to reason about. Overkill here |
| Redis lock / in-memory mutex | Lock outside the database | A Go mutex doesn't work across several API instances. A Redis lock adds a second system that can disagree with the database (what if Redis says "locked" but the DB transaction failed?). The database already has a correct lock |

## What happens during a normal request?

`POST /api/v1/orders` during a rush (100 buyers, stock 1):

1. All 100 requests pass authentication and validation in parallel.
2. They compete for the pool's 10 database connections. The extra requests wait for a free connection. That's normal, and they queue.
3. Each transaction share-locks the product row (they don't block each other) and inserts its order row (new rows, no contention).
4. They reach `SELECT inventory ... FOR UPDATE` on the same row. **One** gets the lock, and the others queue behind it.
5. The first reserves the unit and commits: **201**.
6. Each following transaction wakes up, sees `available = 0`, gets `ErrOutOfStock`, and rolls back, **including its already-inserted order row**. It returns **409 `OUT_OF_STOCK`**.

The live demo measured this at 100 requests in ~0.5 s.

## What happens when it fails?

| Failure | Result |
|---|---|
| Out of stock | 409, and the transaction is rolled back (order and item rows disappear) |
| Deadlock (only possible if someone breaks the lock order) | PostgreSQL aborts one transaction after 1 s with `40P01`, and the API returns 500. `NoDeadlock` catches the regression in tests |
| Client disconnects mid-request | The request context is cancelled, pgx aborts the query, and the transaction rolls back. Locks are released |
| API process crashes while holding locks | The DB connection closes, PostgreSQL rolls back and releases the locks, and waiting transactions continue |
| All pool connections busy | Requests wait for a connection (bounded by the HTTP write timeout). They aren't wrong, just slower |

## What database concepts are involved?

- **ACID**: Atomicity (all or nothing), Consistency (constraints hold after every commit), Isolation (concurrent transactions don't see each other's uncommitted work), Durability (committed data survives a crash).
- **READ COMMITTED**: PostgreSQL's default isolation level. Each *statement* sees data committed before it started. After waiting for a row lock, PostgreSQL re-checks the row's latest committed version.
- **Row-level locks**: `FOR UPDATE` (exclusive) and `FOR SHARE` (shared). Plain `UPDATE`/`DELETE` take exclusive row locks automatically. Plain `SELECT` takes no row locks at all (MVCC: readers never block writers).
- **MVCC** (multi-version concurrency control): PostgreSQL keeps several versions of a row, so plain reads never wait. That's why the *writers* in this project must lock explicitly.
- **Deadlock detection**: `deadlock_timeout` (default 1 s), error `40P01`.
- **Lock ordering**: the standard technique to make deadlocks impossible instead of merely detected.
- **Connection pool as a concurrency limit**: `DB_MAX_CONNS` caps how many transactions run in the database at once.

## What concurrency issues exist?

Everything in this stage is about them. The tests in `internal/orders/concurrency_test.go`:

| Test | Scenario | Guarantee |
|---|---|---|
| `100BuyersOneItem` | stock 1, 100 distinct buyers | 1 ok, 99 out of stock, 1 order row, 1 reservation, stock 0/1 |
| `100BuyersTenItems` | stock 10, 100 buyers | exactly 10 ok |
| `MixedQuantities` | stock 50, 40 buyers asking for 1–4 units | reserved = the winners' demand, available + reserved = 50 |
| `NoDeadlockWithOppositeItemOrder` | 60 three-item orders in 3 different item orders | all 60 succeed, zero deadlocks |
| `CreateAndCancelStorm` | 80 buyers; half the winners cancel immediately | never negative, sum stays 20, invariant holds |
| `ArchiveDuringOrders` | 30 orders racing an admin archive | each order is either complete or rejected |

Plus `tests/concurrency_api_test.go`: the 100-buyer race over **real HTTP**, through auth,
middleware and handlers.

**Reliability.** The service-level tests passed **20 out of 20 runs each with the race detector**
(120 runs in total), and the HTTP test passed 5 out of 5. A concurrency test that passes once
proves little. Run them repeatedly.

## How can I reproduce/test it?

```powershell
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"

# All concurrency tests, 20 times, with Go's race detector
go test -race -count=20 -run Concurrency ./internal/orders/ -v

# Over HTTP
go test -count=3 -run 100Concurrent ./tests/ -v
```

### Live demo against the running API

```powershell
docker compose up --build -d
go run ./cmd/concurrency-demo                        # 100 buyers, stock 1
go run ./cmd/concurrency-demo -buyers 200 -stock 7   # 200 buyers, stock 7
```

Output from this stage's run:

```text
4. GO: 100 buyers x 1 unit(s), all at the same instant

Results (100 requests in 464ms)
   201 CREATED                  1
   409 OUT_OF_STOCK            99

Final inventory: available=0 reserved=1

PASS: exactly 1 order(s) succeeded, the rest got OUT_OF_STOCK, stock never went negative.
```

The demo creates a new `DEMO-<timestamp>` product each run and reuses the
`demo-buyer-NNN@example.com` accounts. It needs an admin account (default `alice@example.com`;
use `-admin-email` / `-admin-password` for another). The winning reservation stays ACTIVE until
it's paid or expires (Stages 9–10).

### Experiments to try

1. **Oversell on purpose.** In `inventory/reservation_repository.go`, remove `FOR UPDATE` from `LockRow`. Then run `go test -count=5 -run 100BuyersOneItem ./internal/orders/`. You'll see several successes, or `version` conflicts, because `Save`'s `WHERE version = $4` is the second safety net. Also remove `AND i.version = $4` from `Save` (and its argument), then run the demo: more than one `201`. Revert everything.
2. **Recreate the deadlock table.** Remove the `slices.SortFunc(...)` line in `ReserveForOrder` and run `go test -count=1 -run NoDeadlock ./internal/orders/ -v`. Count the `40P01` errors and note how long it takes. Revert.
3. **See the queue.** While the demo runs, query `SELECT pid, wait_event_type, wait_event, state, left(query, 60) FROM pg_stat_activity WHERE datname = 'inventory';` in psql. Several sessions show `wait_event_type = Lock` / `transactionid`: those are transactions queued on the row lock.
4. **Shrink the pool.** Restart the API with `DB_MAX_CONNS=1`. The demo is still correct, just slower, because now only one transaction runs at a time. Correctness comes from the locks, not from the number of connections.
5. **Watch `FOR SHARE` block an admin.** In psql, run `BEGIN; SELECT * FROM products WHERE id = 1 FOR SHARE;`. Then PATCH product 1's price through the API: it hangs until you `COMMIT`. Meanwhile an order for product 1 goes through, because shared locks don't block each other.
   - I verified this with a small program. With a shared lock held and an `UPDATE` waiting, a *new* `FOR SHARE` was granted in 7 ms, and the `UPDATE` ran only after the first shared lock was released.
   - **Trade-off (writer starvation):** a steady stream of overlapping orders could, in theory, keep an admin's price change waiting for a long time. Order transactions are a few milliseconds each, so in practice the admin waits briefly. But it's worth knowing that shared locks favour readers.
