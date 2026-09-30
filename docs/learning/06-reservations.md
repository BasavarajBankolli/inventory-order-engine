# 06 — Inventory Reservations

## What problem does this solve?

Between "customer clicks Buy" and "payment succeeds", the units must be **held** for that
customer. Nobody else may buy them, but they aren't sold yet either. If the customer cancels or
never pays, the units must go back on sale.

A **reservation** is that hold: "order #2 holds 2 units of product 1 until 11:40".

## Why do we need it?

| Without reservations | With reservations |
|---|---|
| Check stock, then take payment, then decrement stock. Two customers can both pass the check and both pay for the last unit | Stock is taken **at order time**, inside the order's transaction. The second customer gets `OUT_OF_STOCK` immediately |
| A cancelled or abandoned checkout has to remember what to put back | Each reservation row records exactly what to return, and a status makes sure it's returned only once |
| You can't tell "sold" apart from "waiting for payment" | `available` = can be sold now; `reserved` = held for unfinished orders |

## How does our implementation work?

### Two numbers per product, three moves

```text
                          available   reserved     when
Reserve(3)                   -3          +3        order placed
ReleaseReserved(3)           +3          -3        order cancelled / reservation expired (Stage 10)
ConfirmReserved(3)            0          -3        payment succeeded: units are sold (Stage 9)
```

Example from the spec:

```text
Before:           available = 10, reserved = 0
Reserve 3:        available = 7,  reserved = 3
Release 3:        available = 10, reserved = 0      (cancel)
  — or —
Confirm 3:        available = 7,  reserved = 0      (paid; 3 units left the warehouse)
```

These are methods on `Inventory` in `internal/inventory/reservation.go`. They're plain Go and
all-or-nothing, and `reservation_test.go` unit-tests them. Reserve and Release only **move**
units, so `available + reserved` never changes (`TestReserveReleaseKeepsTotal`).

### The reservation row (`inventory_reservations`)

```text
id  order_id  product_id  quantity  status    expires_at
1   2         1           2         RELEASED  11:40
2   3         5           1         ACTIVE    11:40
```

Reservation statuses:

```text
ACTIVE ──► CONFIRMED   paid                 (Stage 9)
   ├─────► RELEASED    order cancelled      (now)
   └─────► EXPIRED     time ran out         (Stage 10)
```

Only `ACTIVE` rows hold stock. Every change uses `UPDATE ... WHERE id = $1 AND status = 'ACTIVE'`,
so a reservation can be finished **at most once**. That's what keeps stock from being returned twice.

**The invariant.** For every product, at every commit:

```text
inventory.reserved_quantity = SUM(quantity of that product's ACTIVE reservations)
```

`assertInvariant` in `internal/orders/reservation_test.go` checks this with one SQL query after
the tests. You can run the same query yourself (see below).

### Placing an order: one transaction

```text
BEGIN
  SELECT products WHERE id = ANY(...)              current price and status
  buildItems                                       snapshot + total (pure Go)
  INSERT orders (CREATED), INSERT order_items
  inventory.ReserveForOrder(tx, ...)               ◄── runs in the SAME transaction
     for each product, in ascending id order:
       SELECT inventory ... FOR UPDATE             lock the row
       inv.Reserve(q)                              ErrOutOfStock → return error
       UPDATE inventory (available, reserved, version+1)
       INSERT inventory_reservations (ACTIVE, expires_at = now() + 15 min)
  order.TransitionTo(RESERVED); UPDATE orders
COMMIT          ── or ROLLBACK on any error: no order, no items, no reservation, stock untouched
```

A few details:

- **Why does inventory code take the caller's `pgx.Tx`?** Because "order exists" and "stock is reserved" must commit together. If the inventory service opened its own transaction, a crash between the two commits could leave an order without stock, or held stock without an order. `ReserveForOrder(ctx, tx, ...)` joins the transaction the orders service already started.
- **Why lock in ascending product id order?** See "What concurrency issues exist?" below.
- **Why is `expires_at` computed by PostgreSQL** (`now() + make_interval(secs => $4)`)? The worker that expires reservations (Stage 10) compares against the database's `now()`. Using one clock means the app server's clock can't be out of sync with it.
- **The TTL** is configurable with the `RESERVATION_TTL` environment variable (default `15m`).

### Cancelling: release in the same transaction

```text
BEGIN
  SELECT order ... FOR UPDATE                     lock the order
  order.TransitionTo(CANCELLED)                   state machine
  inventory.ReleaseForOrder(tx, orderID, RELEASED)
     SELECT ACTIVE reservations ... FOR UPDATE    (ordered by product id)
     for each: lock inventory row, inv.ReleaseReserved(q), save, reservation → RELEASED
COMMIT
```

`ReleaseForOrder` is **idempotent**. Call it twice and the second call finds no ACTIVE
reservations, so it does nothing. Orders created in Stage 5 (before reservations existed) have
no reservations, and cancelling them releases nothing, which is also correct.

A product that was **archived after the order was placed** still gets its stock back.
`LockRow` deliberately ignores product status (`TestCancel_ReleasesArchivedProduct`).

## What happens during a normal request?

Here's the manual test from this stage (keyboard = product 1, hat = product 5 with exactly 1 unit):

| Step | Result | Keyboard (avail/res) | Hat (avail/res) |
|---|---|---|---|
| Start | | 45 / 0 | 1 / 0 |
| Bob orders 2 keyboards | 201, `RESERVED` | 43 / 2 | 1 / 0 |
| Bob orders the last hat | 201, `RESERVED` | 43 / 2 | 0 / 1 |
| Carol orders a hat | **409 `OUT_OF_STOCK`**: "product 5 has 0 available, 1 requested" | 43 / 2 | 0 / 1 |
| Carol orders 1 keyboard + 1 hat | **409**. The keyboard line was reserved first, then **rolled back** | 43 / 2 | 0 / 1 |
| Bob cancels the keyboard order | 200, `CANCELLED`, reservation → `RELEASED` | 45 / 0 | 0 / 1 |

Bob's hat reservation stays `ACTIVE` until he pays (Stage 9) or it expires (Stage 10).

## What happens when it fails?

| Failure | Result |
|---|---|
| Any line out of stock | 409 `OUT_OF_STOCK`. ROLLBACK undoes the order row, the items, and every line already reserved (`TestReserve_OutOfStockRollsBackEverything`) |
| Crash or lost DB connection mid-transaction | PostgreSQL rolls back the open transaction, so nothing is half-reserved |
| Cancel twice | Second call → 409 `INVALID_STATE_TRANSITION`. Stock is released only once |
| Release called twice directly | The second call returns 0 and changes nothing |
| A bug tries to release more than is reserved | `ErrInconsistentReservation` → 500 + ROLLBACK. The CHECK `reserved_quantity >= 0` would also stop it |
| Customer never pays | The reservation stays `ACTIVE` until the Stage 10 worker expires it and returns the stock |

## What database concepts are involved?

| Concept | Where / why |
|---|---|
| One transaction across two modules | `orders.Service` owns the tx and passes it to `inventory.Service` |
| Row locks `FOR UPDATE` on inventory **and** reservation rows | A reservation can't be released by two transactions at once |
| **Partial index** `ON inventory_reservations (expires_at) WHERE status = 'ACTIVE'` | The expiry worker only ever looks for ACTIVE rows. The index holds only those, so it stays tiny even with millions of finished reservations |
| `UNIQUE (order_id, product_id)` | One reservation per product per order |
| `CHECK (expires_at > created_at)` | A reservation can't be born already expired (a TTL of 0 is rejected) |
| `now()` inside a transaction | Returns the **transaction start time**, so every reservation of one order gets exactly the same `expires_at` |
| Compare-and-set `WHERE status = 'ACTIVE'` | "Finish at most once" without extra locking logic |
| Invariant across tables | Can't be a CHECK constraint, so it's kept by code (one transaction) and verified by a test query |

## What concurrency issues exist?

**Deadlock between two multi-item orders.** Order A wants products 1 and 2; order B wants 2 and 1.
If each locked in the order the customer listed them:

```text
A: lock product 1 ✓           B: lock product 2 ✓
A: lock product 2 ⏳ (B has it) B: lock product 1 ⏳ (A has it)   → deadlock
```

PostgreSQL detects this after `deadlock_timeout` (1 s) and kills one transaction with an error.
We prevent it instead: `ReserveForOrder` **sorts lines by product id** before locking, and
`ReleaseForOrder` reads reservations `ORDER BY product_id`. Every transaction takes locks in the
same global order, so no waiting cycle can form. The overall lock order is:

```text
order row  →  reservation rows  →  inventory rows (ascending product id)
```

Stage 7 adds a test that fires many opposite-order orders at the same time to prove there are no deadlocks.

**Two customers, one unit.** The `FOR UPDATE` on the inventory row makes them take turns. The
first reserves it, and the second then sees `available = 0` and gets `OUT_OF_STOCK`. Stage 7
proves this with 100 simultaneous buyers.

**Cancel racing the expiry worker** (Stage 10). Both lock the order row first, so one waits for
the other. The second sees a final status (CANCELLED/EXPIRED), and its state-machine check fails.
And because reservations finish only from ACTIVE, stock is returned once, whoever wins.

## How can I reproduce/test it?

```powershell
# Unit: stock movements
go test ./internal/inventory/ -run "Reserve|Release|Confirm" -v

# Integration: reserve on create, rollback, release, idempotency, invariant
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 ./internal/orders/ -run "Reserve|Cancel_Releases|ReleaseForOrder|Invariant" -v
go test -count=1 ./tests/ -run ReserveAndRelease -v
```

Manually (with `$admin` and `$bob` tokens; see the README):

```powershell
$base = "http://localhost:8080/api/v1"
Invoke-RestMethod -Uri "$base/products/1/inventory" -Headers $admin
$o = Invoke-RestMethod -Method Post -Uri "$base/orders" -Headers $bob -ContentType "application/json" -Body '{"items":[{"product_id":1,"quantity":2}]}'
Invoke-RestMethod -Uri "$base/products/1/inventory" -Headers $admin          # reserved went up
Invoke-RestMethod -Method Post -Uri "$base/orders/$($o.id)/cancel" -Headers $bob
Invoke-RestMethod -Uri "$base/products/1/inventory" -Headers $admin          # back to where it was
```

The invariant check should always return **0 rows**:

```powershell
docker compose exec postgres psql -U app -d inventory -c "SELECT i.product_id, i.reserved_quantity, COALESCE(SUM(r.quantity),0) AS active_sum FROM inventory i LEFT JOIN inventory_reservations r ON r.product_id = i.product_id AND r.status='ACTIVE' GROUP BY 1,2 HAVING i.reserved_quantity <> COALESCE(SUM(r.quantity),0);"
```

### Experiments to try

1. **Break atomicity.** In `ReserveForOrder`, comment out `repo.CreateReservation(...)`, then place and cancel an order. The cancel releases nothing (no reservation row), so the stock stays reserved forever, and the invariant query shows the product. Revert afterwards.
2. **Watch the rollback.** Give product A 10 units and product B 1 unit. Order `A×2 + B×5`. You get a 409, and A still shows 10 available: its reservation happened inside the transaction, and the transaction was rolled back.
3. **Cause a real deadlock by hand.** Open two psql windows (`docker compose exec postgres psql -U app -d inventory`) and run these steps in order:
   - Window 1: `BEGIN; SELECT * FROM inventory WHERE product_id = 1 FOR UPDATE;`
   - Window 2: `BEGIN; SELECT * FROM inventory WHERE product_id = 2 FOR UPDATE;`
   - Window 1: `SELECT * FROM inventory WHERE product_id = 2 FOR UPDATE;`. It waits.
   - Window 2: `SELECT * FROM inventory WHERE product_id = 1 FOR UPDATE;`

   About 1 second later, one window prints `ERROR: deadlock detected`. Type `ROLLBACK;` in both windows. That's exactly the cycle that sorting by product id prevents in `ReserveForOrder`.
4. **Shorter TTL.** Start the API with `RESERVATION_TTL=1m`, place an order, and look at `expires_at - created_at` in `inventory_reservations`.
