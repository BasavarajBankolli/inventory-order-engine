# 10 — Background Worker: Reservation Expiry & Payment Reconciliation

## What problem does this solve?

A reservation holds stock for an unpaid order. Two things can leave that stock stuck forever:

1. **An abandoned checkout.** The customer reserves the last unit, closes the tab, and never pays. Nobody will ever call the API for that order again.
2. **A payment timeout nobody follows up on.** The order sits in `PAYMENT_PENDING` with an unknown outcome (Stage 9), and the client never retries.

Something has to act **without a request coming in**. That's a background worker: a separate
process that wakes up every few seconds and cleans up.

## Why do we need it?

| Without a worker | With the worker |
|---|---|
| Abandoned carts keep stock reserved forever, so real buyers see `OUT_OF_STOCK` | Stock returns to sale 15 minutes (`RESERVATION_TTL`) after an unpaid order |
| Timed-out payments stay `PAYMENT_PENDING` forever | The worker asks the provider and settles the order: confirmed, cancelled or expired |

When this stage first started, it found **11 ACTIVE reservations** left over from earlier stages'
demos, stock that had been locked away for hours. It released them in its first run.

## How does our implementation work?

### A separate process: `cmd/worker`

```text
docker compose:  postgres → migrate → api
                                    → worker     (same Docker image, command /app/worker)
```

- **It's separate from the API** because background work shouldn't slow down HTTP requests, must keep running when there's no traffic, and can be restarted or scaled on its own.
- **It shares all business logic.** `app.NewWorkerJobs` builds the *same* `orders.Service` the API uses, so the rules (state machine, inventory movements, lock order) exist in exactly one place.
- **`internal/worker.Run`** is a small loop:
  - it runs each job immediately at startup, then every `WORKER_INTERVAL`;
  - a job that errors or panics is logged and retried on the next tick, and the worker keeps running;
  - it stops cleanly on Ctrl+C or `docker stop`.

### The job: `orders.Service.ExpireOverdue`

```text
every WORKER_INTERVAL (10 s):

1. RESERVED orders with an ACTIVE reservation past expires_at        (at most 100 per run)
     for each, in its OWN short transaction:
       lock order → still RESERVED and still expired?   (re-check!)
       RESERVED → EXPIRED
       inventory.ReleaseForOrder(EXPIRED): reserved -= q, available += q, reservation → EXPIRED

2. PAYMENT_PENDING orders whose reservation expired more than PAYMENT_RECONCILE_AFTER (1 min) ago
     for each:
       provider.Status("payment-<id>")        read-only lookup, never charges
         SUCCEEDED          → payment SUCCEEDED, order CONFIRMED, stock sold
         DECLINED           → payment FAILED, order CANCELLED, stock released
         charge not found   → payment FAILED "not_charged_before_expiry",
                               order EXPIRED, stock released
         no answer          → do nothing, try again next run
```

The finder queries use the **partial index** from Stage 6
(`inventory_reservations (expires_at) WHERE status = 'ACTIVE'`). It holds only ACTIVE rows, so
"what has expired?" stays fast no matter how many old reservations exist.

### Idempotency: safe to run twice, or in parallel

The worker **will** do things twice: after a crash, when two copies run, or when the API got
there first. Every step is written so that doing it again changes nothing:

| Protection | What it prevents |
|---|---|
| One transaction **per order**, which locks the order row first | Two workers (or a worker and a customer) working on the same order at the same time |
| **Re-check under the lock** (`status` is still `RESERVED`, reservation is still expired) | Expiring an order the customer paid or cancelled a moment ago |
| Reservations finish only from `ACTIVE` (`WHERE status = 'ACTIVE'`) | Returning the same stock twice |
| Payments are decided only from `PENDING` | Recording a payment result twice |
| State machine (`RESERVED → EXPIRED` allowed; `CONFIRMED → EXPIRED` not) | Any nonsense transition |

`TestExpire_ConcurrentWorkers` runs **5 workers at once** over 10 overdue orders: exactly 10
expirations, and stock is restored exactly once. `TestExpire_RacesWithCancel` races the worker
against the customer's cancel 10 times per run: one of them wins, and stock is released once.

### Payment reconciliation: never expire an order the customer paid for

This is the subtle part.

```text
12:00:00  customer pays → provider CHARGES the card → response lost → 504, order PAYMENT_PENDING
12:15:00  reservation expires
          naive worker: "expired → release stock, EXPIRED"      ❌ customer paid, gets nothing
          our worker:   ask the provider → "SUCCEEDED" → CONFIRMED ✅
```

Three rules make this correct:

1. **Ask, don't guess.** `Provider.Status(key)` is a *read-only* lookup. The worker never charges anyone.
2. **Stop starting charges after expiry.** `/pay` refuses to start (or retry) a charge once the reservation has expired, and returns `409 RESERVATION_EXPIRED`.
3. **Grace period > payment timeout.** A charge started just before expiry could still be in flight. The worker waits `PAYMENT_RECONCILE_AFTER` (1 min), which config **requires** to be longer than `PAYMENT_TIMEOUT` (5 s), so it can be sure no charge is still in progress when it asks.

Rules 2 and 3 together mean that when the worker asks, the provider's answer is final.

**Why the mock moved into a database table.** The Stage 9 mock remembered charges in the API
process's memory, and the worker is a different process. It would never see the API's charges,
would conclude "not charged", and would **expire orders the customer paid for**. Now the mock
keeps its records in `mock_provider_charges`, like a real provider keeps them on its servers. We
caught this while designing the stage, before it could cause harm.

## What happens during a normal request?

There is no request; that's the point. Here's the live run from this stage, with
`RESERVATION_TTL=20s`, `WORKER_INTERVAL=5s` and `PAYMENT_RECONCILE_AFTER=15s`:

| Time | Order A (never paid) | Order B (charged, response lost, never retried) | Keyboard stock |
|---|---|---|---|
| 0 s | ordered 2 → `RESERVED` | ordered 1, pay `TIMEOUT` → 504 → `PAYMENT_PENDING` | 42 / 3 |
| 27 s | **`EXPIRED`**, 2 units back | still `PAYMENT_PENDING` (grace period) | 44 / 1 |
| 44 s | `EXPIRED` | **`CONFIRMED`**: the worker asked the provider, which said SUCCEEDED | 44 / 0 (sold) |

Worker log:

```json
{"msg":"reservation expired; order expired and stock released","service":"worker","order_id":418}
{"msg":"reconcile: provider knows this charge","service":"worker","order_id":419,"result":"SUCCEEDED"}
{"msg":"expire-reservations run finished","expired":0,"confirmed":1,"cancelled":0,"skipped":0,"failed":0}
```

## What happens when it fails?

| Failure | Behaviour |
|---|---|
| One order fails (bug, lock timeout) | Logged and counted as `failed`. Other orders in the batch still run, and the failed one is retried next run |
| The whole job returns an error (DB down) | Logged as "job failed". The worker keeps ticking and recovers when the DB is back |
| A job panics | Recovered, logged with a stack trace, and the worker keeps running |
| Worker crashes mid-order | That order's transaction rolls back and is redone on the next run |
| Provider unreachable during reconciliation | The order is skipped and retried next run. Nothing is guessed |
| Two workers, or worker vs customer | Order row lock + re-check: exactly one acts |
| More than 100 overdue orders | 100 per run. The rest are handled on the following ticks |
| `docker stop` | The context is cancelled, the current transaction finishes or rolls back, and the worker logs "worker stopped" |

## What database concepts are involved?

- **Partial index** for "ACTIVE and expired": small and fast.
- **Batch + per-row transactions**: one bad row doesn't block the batch, and locks are held for milliseconds.
- **Row lock + re-check** ("check again after you get the lock"): the standard way to make background jobs safe against concurrent users.
- **Compare-and-set updates** (`WHERE status = 'ACTIVE'` / `'PENDING'`): each change happens at most once.
- **Database time (`now()`) everywhere**: expiry is decided by one clock, not by each server's.
- **Not used, but worth knowing:** `SELECT ... FOR UPDATE SKIP LOCKED`. With many workers, it lets each one *skip* rows another worker has locked instead of waiting, which spreads the work. We don't need it at this scale, because re-checking makes duplicate attempts harmless.

## What concurrency issues exist?

| Race | Resolution |
|---|---|
| Worker vs worker | Order lock and re-check. The second one sees `EXPIRED` and skips |
| Worker expires while the customer cancels | Both lock the order first. The loser sees a final status, and stock is released once |
| Worker expires while the customer pays | `/pay` step 1 refuses expired reservations, and the worker only touches `RESERVED` orders in step 1. Only one can start |
| Worker reconciles while a charge is in flight | Prevented by the grace period being longer than the payment timeout, plus the no-new-charges-after-expiry rule |
| Worker vs checkout rush on the same product | Lock order is always order → reservations → inventory by product id, the same as the API, so no deadlocks |

## How can I reproduce/test it?

```powershell
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test ./internal/worker/ -v                                  # loop, errors, panics, stop
go test -count=1 -run "Expire|Reconcile" ./internal/orders/ -v # all expiry and reconciliation paths
go test -race -count=10 -run "Expire_ConcurrentWorkers|Expire_RacesWithCancel" ./internal/orders/
```

Watch it live with short timings:

```powershell
$env:RESERVATION_TTL = "20s"; $env:WORKER_INTERVAL = "5s"; $env:PAYMENT_RECONCILE_AFTER = "15s"
docker compose up --build -d
docker compose logs -f worker
# in another window: place an order and don't pay it. About 25 s later it's EXPIRED.

# afterwards, go back to the defaults:
Remove-Item Env:RESERVATION_TTL, Env:WORKER_INTERVAL, Env:PAYMENT_RECONCILE_AFTER
docker compose up -d
```

Run the worker outside Docker (for breakpoints):

```powershell
$env:DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory?sslmode=disable"
go run ./cmd/worker
```

### Experiments to try

1. **Kill it mid-run.** With short timings (see above), create a batch of orders and don't pay them. When they're about to expire, run `docker compose kill worker` (a hard kill, no graceful stop), then `docker compose start worker`. Now run the invariant query from [06-reservations.md](06-reservations.md): it returns 0 rows. Any order the worker was halfway through was rolled back by PostgreSQL and handled again after the restart.
   - Also count the guards in `expireReserved`: order lock, status re-check, expiry re-check, state machine, and reservations finishing only from ACTIVE. Several independent safety nets is normal for code that moves stock.
2. **The naive worker.** In `reconcile`, replace the provider lookup with a direct call to `expireUncharged`. Run `TestReconcile_ChargedTimeoutIsConfirmed`: it fails because a paid order is expired. That's exactly the bug reconciliation prevents.
3. **Break the config rule.** Start the worker with `PAYMENT_TIMEOUT=30s` and `PAYMENT_RECONCILE_AFTER=10s`. It refuses to start, and explains why.
4. **Two workers.** Run `docker compose up -d --scale worker=2`, create many expiring orders, and check that each order is expired exactly once: `SELECT order_id, count(*) FROM inventory_reservations WHERE status='EXPIRED' GROUP BY 1 HAVING count(*) > 1;` should return 0 rows. (Scale back with `--scale worker=1`.)
