# 12 — The Transactional Outbox

## What problem does this solve?

Other systems need to know what happened here:
- an email service sends "order confirmed";
- the warehouse starts packing;
- analytics counts sales.

The usual way is to publish **events** to a message broker (Kafka, RabbitMQ, SNS). The hard part
is doing it **reliably**, because a database commit and a broker publish are two separate systems:

```text
Option A: commit, then publish
  COMMIT order                     ✓
  broker.Publish(OrderConfirmed)   ✗ crash / network blip   → the event is LOST. The warehouse never ships.

Option B: publish, then commit
  broker.Publish(OrderConfirmed)   ✓
  COMMIT order                     ✗ deadlock / constraint   → an event about an order that DOESN'T EXIST
```

This is the **dual-write problem**. There's no transaction that spans PostgreSQL and Kafka.

## Why do we need it?

The outbox avoids the second system at write time. **The event is just another row**, inserted
in the **same PostgreSQL transaction** as the business change:

```text
BEGIN
  INSERT orders ...                     the change
  UPDATE inventory ...
  INSERT outbox_events (OrderCreated)   the event, same transaction
COMMIT                                  both or neither
```

A background job then reads committed events and publishes them. Result:

| Guarantee | Why |
|---|---|
| **Never lost** | The event is committed with the change. If publishing fails, the row stays `PENDING` and is retried |
| **Never invented** | If the transaction rolls back, the event row rolls back too |
| **In order per order** | See "ordering" below |
| **At least once** (not exactly once) | See "at-least-once delivery" below |

## How does our implementation work?

### The table (`migrations/000010_create_outbox_events.sql`)

| Column | Purpose |
|---|---|
| `event_type`, `aggregate_type`, `aggregate_id`, `payload` (JSONB) | What happened, to which order, and the details |
| `status` | `PENDING` → `PROCESSED`, or `FAILED` (dead letter) |
| `attempts`, `next_attempt_at`, `last_error` | Retry bookkeeping (additions to the spec's columns) |
| `created_at`, `processed_at` | When it happened / when it was delivered |

There are two **partial indexes** (`WHERE status = 'PENDING'`). The publisher only ever looks at
pending rows, and old processed rows don't bloat the index.

### Where events are written (`internal/orders/events.go`)

| Event | Written by (same transaction) | Payload |
|---|---|---|
| `OrderCreated` | `createInTx` | order_id, user_id, total, currency, items |
| `InventoryReserved` | `createInTx` | order_id, items, expires_at |
| `OrderConfirmed` | `completePayment` (success) | order_id, payment_id, provider_reference, amount |
| `OrderCancelled` | `Cancel` / `completePayment` (decline) | order_id, reason: `customer_cancelled` / `payment_declined` |
| `OrderExpired` | worker `expireReserved` / `expireUncharged` | order_id, reason: `reservation_expired` / `not_charged_before_expiry` |

Business code only ever calls `s.emit(ctx, tx, type, orderID, payload)`, which uses
`outbox.WithTx(tx)`. It *can't* write an event outside the transaction.

A payment **timeout** changes nothing, so it emits nothing (`TestEvents_PaymentOutcomes`).

### Publishing (`internal/events/processor.go`, worker job `publish-outbox`)

```text
every WORKER_INTERVAL (10 s), repeated until nothing more is due (Drain):
  BEGIN
    SELECT ... FROM outbox_events e
    WHERE status = 'PENDING' AND next_attempt_at <= now()
      AND NOT EXISTS (older PENDING event of the same order)     ← ordering
    ORDER BY id LIMIT 100
    FOR UPDATE SKIP LOCKED                                       ← many workers
    for each event:
      publisher.Publish(event)  (5 s timeout)
        ok   → status PROCESSED, processed_at = now()
        fail → attempts+1, last_error, next_attempt_at = now() + backoff
               attempts reached OUTBOX_MAX_ATTEMPTS (10)? → status FAILED (dead letter)
  COMMIT
```

**The publisher.** `LogPublisher` writes the full message as JSON to the worker log. It stands
in for Kafka; a real one would be one new type implementing `Publisher`. `OUTBOX_FAILURE_RATE`
makes it fail on purpose so you can watch retries.

**Exponential backoff:** 2 s, 4 s, 8 s, 16 s, 32 s … capped at 5 minutes. A struggling broker
gets room to recover instead of being hammered by retries.

**Dead letter.** After 10 failed attempts an event becomes `FAILED` and is no longer retried. A
human looks at `last_error`, fixes the cause, and requeues it with
`UPDATE outbox_events SET status='PENDING', attempts=0, next_attempt_at=now() WHERE id=...`.

### Ordering: events of one order arrive in order

A consumer must never see `OrderConfirmed` before `OrderCreated`. Two rules make sure it doesn't:

1. **`NOT EXISTS (older PENDING event of the same order)`**: only the *oldest* pending event of each order can be claimed. If `OrderCreated` fails and waits 32 s for its retry, that order's later events wait too. **Other orders are unaffected.**
2. **This holds across workers.** While worker A holds `OrderCreated` locked, it's still `PENDING`, so worker B's `NOT EXISTS` sees it and skips the order's later events.

Because each batch holds at most one event per order, `Drain` runs batches back to back. The
order's next event becomes eligible as soon as the previous one commits, so all three events of
an order go out in one worker run.

### Several workers: `FOR UPDATE SKIP LOCKED`

| Locking | Two workers at once |
|---|---|
| None | Both read the same rows, so **every event is published several times**. Measured: each event 3 times with 5 workers (`TestConcurrentProcessors` fails) |
| `FOR UPDATE` | The second worker **waits** for the first. Correct, but only one works at a time |
| `FOR UPDATE SKIP LOCKED` | The second worker **skips** rows the first has locked and takes others. Correct *and* parallel |

### At-least-once delivery, and why consumers must be idempotent

```text
publish(OrderConfirmed)  ✓
UPDATE status = PROCESSED
COMMIT                   ✗ crash here → the row is still PENDING → published AGAIN later
```

We can't make "publish" and "mark processed" atomic either, because that's the dual-write
problem again. So we choose the safe side: an event may be delivered **twice**, but never
**zero** times. Every message carries its outbox `id`, and consumers record the ids they've
handled and ignore repeats. That's the same idempotency idea as Stage 8, now on the receiving
side.

## What happens during a normal request?

This is the live run from this stage. Bob orders and pays:

```text
right after the API commits:          1 OrderCreated PENDING, 2 InventoryReserved PENDING, 3 OrderConfirmed PENDING
~5 s later (next worker tick, Drain):  all three PROCESSED, published in id order in ONE run
```

Worker log (shortened):

```json
{"msg":"event published","event_type":"OrderCreated","message":{"id":1,"type":"OrderCreated","aggregate_id":528,"payload":{...}}}
{"msg":"event published","event_type":"InventoryReserved","message":{"id":2,...}}
{"msg":"event published","event_type":"OrderConfirmed","message":{"id":3,...,"provider_reference":"mock_ch_..."}}
```

With a flaky broker (`OUTBOX_FAILURE_RATE=0.5`), 5 orders placed and cancelled (15 events):

| After | PROCESSED | PENDING (retrying or waiting their turn) | Ordering violations |
|---|---|---|---|
| 8 s | 5 | 10 | 0 |
| 20 s | 8 | 7 | 0 |
| 45 s | 10 | 5 (backoff up to 32 s) | 0 |
| later | **15** | 0 | 0 |

## What happens when it fails?

| Failure | Result |
|---|---|
| Business transaction rolls back (e.g. out of stock) | No event (`TestEvents_FailedCreateEmitsNothing`) |
| Broker down | Events stay `PENDING`, retried with backoff. Checkout keeps working, because only outbox rows are locked, never orders or inventory |
| Broker down for a long time | After 10 attempts → `FAILED` (dead letter) + ERROR log |
| Worker crashes after publish, before commit | The event is published again (at-least-once) |
| Worker down | Events pile up as `PENDING` and all go out when it's back, in order |
| Two or more workers | `SKIP LOCKED`: each event is published once |

## What database concepts are involved?

- **Transactional outbox**: the event as a row in the business transaction.
- **`FOR UPDATE SKIP LOCKED`**: the standard way to build a work queue on PostgreSQL.
- **`NOT EXISTS` anti-join** for per-entity ordering.
- **Partial indexes** on `status = 'PENDING'`.
- **JSONB** payloads: flexible per event type, and still queryable (`payload->>'reason'`).
- **Exponential backoff** with `now() + make_interval(...)`.
- **Dead-letter status** for poison events.
- **Not used, but a common upgrade:** `LISTEN/NOTIFY` to wake the publisher right after a commit instead of polling every 10 s. Change-data-capture tools (Debezium) read the table from PostgreSQL's WAL instead.

## What concurrency issues exist?

| Issue | Handling | Test |
|---|---|---|
| Several workers publish the same event | `FOR UPDATE SKIP LOCKED` | `TestConcurrentProcessors`: 60 events, 5 workers, each published exactly once (20/20 runs). Without locking: 3× each |
| Out-of-order delivery after a retry | `NOT EXISTS` older pending | `TestOrderingPerAggregate`, and the live run showed 0 violations |
| Expiry worker emits `OrderExpired` twice | Expiry is idempotent (Stage 10), so the event is written once | `TestEvents_ExpiryEmitsOrderExpired` |
| Slow broker blocks checkout | Only outbox rows are locked, and each publish has a 5 s timeout | – |

## How can I reproduce/test it?

```powershell
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 ./internal/events/ -v                       # outbox mechanics
go test -count=1 -run "Events_" ./internal/orders/ -v        # which events the orders module writes
go test -race -count=20 -run "Concurrent|Ordering" ./internal/events/
```

Watch it live:

```powershell
docker compose logs -f worker | Select-String "event published|will retry|dead letter"
docker compose exec postgres psql -U app -d inventory -c "SELECT id, event_type, aggregate_id, status, attempts, last_error FROM outbox_events ORDER BY id DESC LIMIT 10;"
```

A flaky broker:

```powershell
$env:OUTBOX_FAILURE_RATE = "0.5"; $env:WORKER_INTERVAL = "2s"; docker compose up -d worker
# place and cancel a few orders, then watch the outbox table
Remove-Item Env:OUTBOX_FAILURE_RATE, Env:WORKER_INTERVAL; docker compose up -d worker   # back to normal
```

### Experiments to try

1. **See the dual-write bug.** In `createInTx`, move the two `s.emit(...)` calls *after* `database.WithTx` returns, using `s.outbox.Add` on the pool. Then make `ReserveForOrder` fail (order more than is in stock): no event, fine. Now make the *emit* fail instead (temporarily return an error from `Add`): the order exists but its event doesn't. That's exactly the gap the outbox closes. Revert.
2. **Break ordering.** Remove the `NOT EXISTS` clause and run `TestOrderingPerAggregate`: `OrderConfirmed` gets published before `OrderCreated`.
3. **Break concurrency.** Remove `FOR UPDATE SKIP LOCKED` and run `TestConcurrentProcessors`: events are published several times.
4. **Dead letter.** Run with `OUTBOX_FAILURE_RATE=1` and `OUTBOX_MAX_ATTEMPTS=3`, then place an order. After about 14 s (2 + 4 + 8), its first event is `FAILED` and its later events stay `PENDING` behind it. Requeue it with the `UPDATE` from above.
5. **Be the consumer.** Write a small script that reads `event published` lines from the worker log and prints "email sent for order X", skipping event ids it has already seen. That's an idempotent consumer.
