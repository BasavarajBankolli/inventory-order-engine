# 08 — Idempotency Keys

## What problem does this solve?

```text
Client ── POST /orders ──►  Server creates order #123, reserves stock
Client ◄──── ✗ ──────────  Response lost (timeout, Wi-Fi drop, load balancer restart)
Client ── POST /orders ──►  Retry. Without protection: order #124 + stock reserved AGAIN
```

From the client's side, "the request failed" and "the request succeeded but I never heard back"
look **exactly the same**. Retrying is the only sensible reaction, and without idempotency every
retry risks a duplicate order, a double charge (Stage 9), and stock held twice.

**Idempotent** means: doing it twice has the same effect as doing it once.

## Why do we need it?

`GET`, `PUT` and `DELETE` are naturally idempotent. `POST /orders` isn't: each call creates
something new. The standard fix, used by Stripe, PayPal and AWS, is an **idempotency key**. The
client generates a unique value (usually a UUID) **once per checkout** and sends it with every
attempt:

```http
POST /api/v1/orders
Idempotency-Key: 3f0c7b8e-1d2a-4c55-9e1b-0a6f2d3c4b5a
```

The server stores the key together with the result. Any later request with the same key gets
the **original result** instead of a new order.

## How does our implementation work?

### Storage: two columns and one unique index (migration 000007)

```sql
ALTER TABLE orders ADD COLUMN idempotency_key TEXT, ADD COLUMN request_hash TEXT;

CREATE UNIQUE INDEX orders_user_idempotency_key_idx
    ON orders (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
```

| Piece | Why |
|---|---|
| Key stored **on the order row** | The key and the order are written in the **same transaction**, so there can never be a key without its order, or an order that lost its key |
| Unique per **(user_id, key)** | Clients choose keys. Two customers who both send `retry-1` must not see each other's orders |
| `request_hash` = SHA-256 of the basket | Detects a client bug: the same key reused for a *different* order |
| Partial index (`WHERE ... IS NOT NULL`) | Orders placed without a key aren't indexed, which keeps the index small |
| `CHECK ((idempotency_key IS NULL) = (request_hash IS NULL))` | A key is always stored with its hash, and never one without the other |

### The flow (`orders.Service.CreateWithKey`)

```text
validate items, validate key (1-255 visible ASCII characters)
hash = fingerprint(items)                          sorted "productID:qty;" → SHA-256

1. FAST PATH: is there already an order for (user, key)?
     same hash       → return it          → 200 OK + Idempotent-Replayed: true
     different hash  → 409 IDEMPOTENCY_KEY_REUSED
     none            → continue

2. BEGIN
     products FOR SHARE, buildItems
     INSERT order (..., idempotency_key, request_hash)   ◄── the unique index decides here
     reserve stock, CREATED → RESERVED
   COMMIT                                                → 201 Created

3. RACE PATH: the INSERT failed with a unique violation (another request with the
   same key committed first) → our transaction rolls back → do step 1 again and
   return the winner's order                                → 200 OK (replay)
```

HTTP responses:

| Situation | Status | Body | Headers |
|---|---|---|---|
| New order | `201 Created` | the new order | `Location` |
| Retry of the same request | `200 OK` | the **original** order | `Location`, `Idempotent-Replayed: true` |
| Same key, different basket | `409 Conflict` | `IDEMPOTENCY_KEY_REUSED` | |
| Malformed key | `400` | `fields: {"Idempotency-Key": ...}` | |
| No key | normal behaviour, no de-duplication | | |

### Why isn't "check first, then insert" enough? The race

```text
t0  A: fast path: SELECT order WHERE key = K  → none
t1  B: fast path: SELECT order WHERE key = K  → none      (A hasn't committed)
t2  A: INSERT order (key K) ...
t3  B: INSERT order (key K) ...                             → two orders!  ❌
```

Every "look before you leap" check has this gap. **The unique index closes it:**

```text
t2  A: INSERT order (key K)     → index entry for (user, K), not committed yet
t3  B: INSERT order (key K)     → PostgreSQL sees A's pending entry and WAITS ⏳
t4  A: reserve stock, COMMIT
t5  B: → unique_violation (23505) → our code: errDuplicateIdempotencyKey
        → B's transaction rolls back (B never reached the inventory lock)
        → B re-runs the fast path, finds A's order → 200 replay     ✅
```

If A had **rolled back** instead (for example out of stock), B's INSERT would simply succeed and
B would try again. Nothing is lost.

Notice where the INSERT sits: the order row is inserted **before** stock is reserved. A losing
duplicate waits at the INSERT and never touches `inventory`, so stock is reserved exactly once
even in the race.

**Measured.** `TestIdempotency_ConcurrentDuplicates` fires 20 identical requests at once:

| Version | Orders created | Units reserved |
|---|---|---|
| With the unique index (our code) | **1** (plus 19 replays) | **2** |
| Unique index replaced by a normal index (experiment) | 16–19 | 32–38 |

The Go fast-path check alone let 16–19 duplicates through. The database constraint is what
actually makes it safe.

### Design choices

- **Only successful orders store their key.** If an attempt fails (for example `OUT_OF_STOCK`), no row exists, so a retry with the same key is a genuinely new attempt that may now succeed (`TestIdempotency_FailedAttemptCanBeRetried`). Stripe also caches errors. For orders, "retry after an error should be able to succeed" is the friendlier behaviour.
- **The fingerprint ignores line order**: `[mug, pen]` and `[pen, mug]` are the same basket.
- **Replays return the order as it is *now*.** If the order was cancelled in between, the replay shows `CANCELLED`. That's the truth, and it's what a client needs to know.
- **Keys never expire.** They live on the order row forever. Real systems often let keys expire after 24 hours; see the "Future improvements" section in Stage 15.
- **The key is optional.** Clients that don't send one get normal behaviour. A stricter API could require it for `POST /orders`.

## What happens during a normal request?

1. The client generates `3f0c7b8e-...` when the customer opens the checkout page, and sends `POST /orders` with that key.
2. The fast path finds nothing, so the order is created and stored with the key and hash, and the API returns **201**.
3. The response is lost, so the client retries with the same key and body.
4. The fast path finds the order and the hashes match, so the API returns **200** with `Idempotent-Replayed: true`. There's no new order and no new reservation.
5. The log line says `"idempotent replay: returning existing order" order_id=... request_id=...`.

## What happens when it fails?

| Failure | Result |
|---|---|
| Response lost after COMMIT | The retry gets the original order (200) |
| Crash **before** COMMIT | Nothing was stored, and the retry creates the order (201) |
| Two copies arrive at the same moment | One 201, the others wait on the unique index, then get a 200 replay |
| Same key, different basket | 409 `IDEMPOTENCY_KEY_REUSED`. The client has a bug and must use a new key for a new order |
| First attempt `OUT_OF_STOCK`, then restocked | The retry with the same key succeeds (201) |
| Key with spaces, or longer than 255 characters | 400 |
| Same key from two different users | Two independent orders |

## What database concepts are involved?

- **Unique index as a concurrency tool**: the database, not application code, decides who wins.
- **Unique checks wait on uncommitted rows**: an INSERT that conflicts with an *in-progress* transaction's row waits for it to finish, then either fails (the other committed) or succeeds (the other rolled back).
- **Partial unique index**: uniqueness only where `idempotency_key IS NOT NULL`.
- **Error code `23505` unique_violation**: `database.IsUniqueViolation(err, "orders_user_idempotency_key_idx")` recognises this specific index.
- **Online schema change**: `ALTER TABLE ... ADD COLUMN` (nullable, no default) is instant in PostgreSQL. On a large production table, the index would be built with `CREATE UNIQUE INDEX CONCURRENTLY`.
- **Cross-column CHECK** (`(key IS NULL) = (hash IS NULL)`).

## What concurrency issues exist?

The race covered above is the main one, and it's handled by the unique index. Also:

- **Duplicates racing for the last unit** (`ConcurrentDuplicatesForLastUnit`). Ten copies of "buy the last one" must **not** produce one success and nine `OUT_OF_STOCK`. They're the *same* purchase, so all ten get the same successful order. That works because a duplicate stops at the order INSERT and never reaches the inventory lock.
- **Many different users racing** is still handled by the Stage 7 row lock. Idempotency and stock locking are independent layers.

## How can I reproduce/test it?

```powershell
# Unit: fingerprint and key validation
go test ./internal/orders/ -run "Fingerprint|ValidateIdempotencyKey" -v

# Integration and HTTP, including the concurrent-duplicate race
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -race -count=10 -run "Idempotency" ./internal/orders/ ./tests/ -v

# Live demo: one buyer, one key, 50 simultaneous copies
go run ./cmd/concurrency-demo -mode idempotency -buyers 50
```

Demo output from this stage:

```text
Results (50 requests in 258ms)
   201 CREATED (new order)          1
   200 OK (idempotent replay)      49
   distinct order ids returned      1  map[306:50]
Final inventory: available=0 reserved=1
PASS: one order was created; every retry got that same order back; stock was reserved once.
```

Manually in PowerShell (use `Invoke-WebRequest` to see the status code and headers):

```powershell
$base = "http://localhost:8080/api/v1"
$h = @{ Authorization = "Bearer $token"; "Idempotency-Key" = [guid]::NewGuid().ToString() }
$body = '{"items":[{"product_id":1,"quantity":1}]}'

$r1 = Invoke-WebRequest -Method Post -Uri "$base/orders" -Headers $h -ContentType "application/json" -Body $body -UseBasicParsing
$r2 = Invoke-WebRequest -Method Post -Uri "$base/orders" -Headers $h -ContentType "application/json" -Body $body -UseBasicParsing
$r1.StatusCode; $r2.StatusCode; $r2.Headers["Idempotent-Replayed"]   # 201, 200, true
($r1.Content | ConvertFrom-Json).id; ($r2.Content | ConvertFrom-Json).id  # same id
```

### Experiments to try

1. **Remove the database guarantee.** In `migrations/000007_add_order_idempotency.sql`, change `CREATE UNIQUE INDEX` to `CREATE INDEX`, then run `go test -count=3 -run Idempotency_ConcurrentDuplicates$ ./internal/orders/`. You'll see around 16–19 orders instead of 1. The test database is migrated fresh on every run, so this is safe. Revert afterwards. **Never edit an applied migration in real life.** This is only for the throwaway test schema.
2. **Remove the race path.** In `CreateWithKey`, delete the `if errors.Is(err, errDuplicateIdempotencyKey)` block. The concurrent test still creates only 1 order, but the losers now get **500 errors** instead of replays. That shows why the service must *handle* the unique violation, not just rely on it.
3. **Key reuse.** Send the same key with `quantity: 1`, then with `quantity: 2`. You get 409 `IDEMPOTENCY_KEY_REUSED`.
4. **Per-user keys.** Log in as two different customers and send the same key from both. You get two separate orders.
5. **Look at the data.** Run `SELECT id, user_id, idempotency_key, left(request_hash, 12) FROM orders WHERE idempotency_key IS NOT NULL;`.
