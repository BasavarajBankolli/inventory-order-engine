# 09 — Mock Payments

## What problem does this solve?

A reserved order has to be **paid** before its stock counts as sold. Payment means calling an
external system, a payment provider such as Stripe or Razorpay, and that call is:

- **slow**: hundreds of milliseconds to several seconds;
- **unreliable**: it can time out, and the network can drop the response;
- **irreversible**: a database ROLLBACK can't take a card charge back.

This stage adds the payment step with a **mock provider** that can be told to succeed, fail or
time out, so every path can be tested without real money.

## Why do we need it?

| Result | What must happen to the order | What must happen to the stock |
|---|---|---|
| Payment succeeds | `CONFIRMED` | Reserved units become **sold** (`reserved -= q`) |
| Payment declined | `PAYMENT_FAILED → CANCELLED` | Reserved units go **back on sale** (`available += q`) |
| No answer (timeout) | Must **not** be cancelled, because the money may have been taken | Must stay reserved |

The third row is the one most systems get wrong.

## How does our implementation work?

### The provider interface (`internal/payments/provider.go`)

```go
type Provider interface {
    Charge(ctx context.Context, req ChargeRequest) (Result, error)
}
type ChargeRequest struct {
    Amount         money.Money   // {Amount: 199900, Currency: "INR"}
    IdempotencyKey string        // "payment-<payments.id>"
}
```

- **A deliberate change from the spec**, which had `Charge(ctx, amount Money)`. Real providers accept an idempotency key, so a *retried* charge returns the original result instead of charging again. Without the key, "retry after a timeout" can mean "charge the customer twice". The key is derived from our payment row id, so every retry of one payment uses the same key.
- **Decline vs error.** A decline is a normal `Result{Status: DECLINED}`: the provider answered "no". An `error` means **we don't know** what happened.
- **Swappable.** Orders only know the interface. A real `StripeProvider` would be one new type plus one line in `internal/app/app.go`.

### The mock (`internal/payments/mock.go`)

| Outcome | What the mock does |
|---|---|
| `SUCCESS` | Takes the money and returns a reference `mock_ch_...` |
| `FAILURE` | Returns `DECLINED` / `card_declined`. No money taken |
| `TIMEOUT` | **Takes the money**, then never answers (waits until our deadline). This is the realistic worst case: "charged, but the response was lost" |

It's idempotent per key, like a real provider: the first request for a key decides the result,
and later ones get the same result without charging again. It counts real charges (`Charges()`)
so tests can assert "charged exactly once".

> **Updated in Stage 10:** the mock keeps its charges in its own table, `mock_provider_charges`
> (migration 000009), instead of in memory. A real provider keeps records on *its* servers, which
> every client can query. The API and the worker are separate processes, and both must see the
> same charges, so the worker can ask "did that timed-out charge go through?" through
> `Provider.Status`. An in-memory map inside the API process can't do that.

Choose the outcome with:
- `MOCK_PAYMENT_OUTCOME` (default `SUCCESS`), or
- per request: `POST /orders/{id}/pay` with body `{"simulate": "FAILURE"}`. This goes through the context as a clearly marked test hook, and a real provider would ignore it.

### The pay flow (`internal/orders/payment.go`): three steps, two short transactions

```text
STEP 1  BEGIN
          SELECT order FOR UPDATE
          RESERVED → PAYMENT_PENDING                      (state machine)
          reservation expired? → 409 RESERVATION_EXPIRED
          INSERT payments (PENDING)                       (row exists BEFORE we call out)
        COMMIT                                            ← locks released

STEP 2  provider.Charge(ctx with 5 s timeout, key = "payment-<id>")    NO transaction, NO locks

STEP 3  BEGIN
          SELECT order FOR UPDATE
          SUCCEEDED → payment SUCCEEDED + reference, order CONFIRMED,
                      inventory.ConfirmForOrder: reserved -= q, reservations CONFIRMED
          DECLINED  → payment FAILED, order PAYMENT_FAILED → CANCELLED,
                      inventory.ReleaseForOrder: available += q, reservations RELEASED
        COMMIT
        (no answer → skip step 3: order stays PAYMENT_PENDING, 504 PAYMENT_TIMEOUT)
```

**Why not one big transaction?**

1. Holding the inventory row lock for a multi-second remote call would make every other buyer of that product wait, which ruins the throughput we measured in Stage 7.
2. A transaction can't undo a card charge anyway.

**Why insert the PENDING payment before calling the provider?** If the process crashes during
step 2, there's still a record that a charge *may* exist. That record carries the key a retry
(or a reconciliation job) needs.

### Why a timeout must NOT cancel the order

```text
Provider: charge ✓ ... response lost
Us:       timeout → "failed"? → cancel order, release stock    ❌  customer paid, gets nothing
```

What we do instead:
- The order stays `PAYMENT_PENDING`, the payment stays `PENDING`, and the stock stays reserved.
- The client gets **504 `PAYMENT_TIMEOUT`**: "retry, you will not be charged twice".
- The retry reuses the same payment row and key. The provider replies "already succeeded", and the order becomes `CONFIRMED`.
- The customer can't cancel while `PAYMENT_PENDING`: the state machine has no `PAYMENT_PENDING → CANCELLED` edge (Stage 5).

What if the client never retries? Stage 10's worker deals with orders stuck in `PAYMENT_PENDING`.
A production system would also run a **reconciliation** job that asks the provider for the
truth about old PENDING payments.

### Pay is idempotent too

| Order status when `/pay` is called | Behaviour |
|---|---|
| `RESERVED` | First attempt: create a payment and charge |
| `PAYMENT_PENDING` | Retry: same payment, same key, so the provider returns the original result |
| `CONFIRMED` | Already paid: return the stored result, no provider call |
| `CANCELLED`, `EXPIRED`… | 409 `INVALID_STATE_TRANSITION` |

## What happens during a normal request?

Here's the manual run from this stage (Bob, product 2, starting at available=20, reserved=0):

| Case | Response | Order | Payment | Stock (avail/res) |
|---|---|---|---|---|
| A) Order 2 units, pay | 200 | `CONFIRMED` | `SUCCEEDED`, `mock_ch_…_1` | 18/2 → **18/0** (sold) |
| B) Order 3, pay with `FAILURE` | **402 `PAYMENT_FAILED`** | `CANCELLED` | `FAILED`, `card_declined` | 15/3 → **18/0** (restored) |
| C) Order 1, pay with `TIMEOUT` | **504 `PAYMENT_TIMEOUT`** after 5 s | `PAYMENT_PENDING` | `PENDING` | 17/1 (unchanged) |
| C) Retry pay | 200 | `CONFIRMED` | `SUCCEEDED`, `mock_ch_…_2` | **17/0** |

The `_2` suffix is the mock's charge counter. Across the whole run, money was taken exactly
twice (orders A and C), **not** three times. The timeout retry didn't double-charge.

## What happens when it fails?

| Failure | Result |
|---|---|
| Card declined | 402. Order cancelled, stock released, payment `FAILED` with a reason |
| Provider timeout / network error | 504. Nothing changes; retry is safe |
| Client disconnects during step 2 | Same as a timeout: the context is cancelled, the order stays `PAYMENT_PENDING` |
| Crash between step 2 and 3 | The payment row stays `PENDING` with its key; the next `/pay` retry discovers the result |
| Reservation already expired | 409 `RESERVATION_EXPIRED`. **No charge is attempted** |
| Order no longer payable when success arrives (shouldn't happen) | The payment is still recorded as `SUCCEEDED` (money is never lost track of), and an ERROR log says `REFUND REQUIRED` |
| Ten "Pay" clicks at once | One payment row, one charge, all ten get the confirmed order |

## What database concepts are involved?

`migrations/000008_create_payments.sql`:

| Constraint | Why |
|---|---|
| `UNIQUE (order_id)` | One payment per order. A retry reuses it; a second concurrent `/pay` can't create another |
| `UNIQUE (provider_reference)` | One provider charge can't be booked against two payments |
| `CHECK (status <> 'SUCCEEDED' OR provider_reference IS NOT NULL)` | A successful payment must say *which* charge took the money (needed for refunds and disputes) |
| `CHECK (status IN (...))`, `amount > 0`, currency format | Only valid rows |

Also used:
- **Compare-and-set**: `UPDATE payments ... WHERE id = $1 AND status = 'PENDING'`. A payment is decided at most once.
- **Multiple short transactions** instead of one long one, with a durable `PENDING` row as the bridge between them.
- **Row lock on the order** in both transactions, so step 3 can't race a cancel or a second `/pay`.

## What concurrency issues exist?

- **Double click on "Pay"** (`TestPay_ConcurrentClicksChargeOnce`, 20/20 runs). The first transaction moves the order to `PAYMENT_PENDING` and creates the payment. The others wait on the order lock, then see `PAYMENT_PENDING` and reuse the same payment and key. The provider charges once, the first step 3 confirms, and the later ones see `CONFIRMED` and return it.
- **Cancel vs pay.** Cancel is impossible in `PAYMENT_PENDING`, and both lock the order row.
- **Expiry vs pay** (Stage 10). Step 1 refuses expired reservations *before* charging. The worker must never expire an order whose payment might still succeed; Stage 10 is designed around that.
- **Locks are never held during the provider call**, so a slow provider slows down only its own request.

## How can I reproduce/test it?

```powershell
# Unit: mock provider and money formatting
go test ./internal/payments/ ./internal/money/ -v

# Integration and HTTP
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 -run "Pay" ./internal/orders/ -v
go test -count=1 -run "Payments" ./tests/ -v
```

Manually (with `$bob` as a customer token; see the README):

```powershell
$base = "http://localhost:8080/api/v1"
$o = Invoke-RestMethod -Method Post -Uri "$base/orders" -Headers $bob -ContentType "application/json" -Body '{"items":[{"product_id":2,"quantity":1}]}'

# Success
Invoke-RestMethod -Method Post -Uri "$base/orders/$($o.id)/pay" -Headers $bob

# Failure (on a new order): 402, order cancelled, stock restored
try { Invoke-RestMethod -Method Post -Uri "$base/orders/$($o.id)/pay" -Headers $bob -ContentType "application/json" -Body '{"simulate":"FAILURE"}' } catch { $_.ErrorDetails.Message }

# Timeout (on a new order): 504 after 5 s, then call /pay again: CONFIRMED
try { Invoke-RestMethod -Method Post -Uri "$base/orders/$($o.id)/pay" -Headers $bob -ContentType "application/json" -Body '{"simulate":"TIMEOUT"}' } catch { $_.ErrorDetails.Message }

docker compose exec postgres psql -U app -d inventory -c "SELECT id, order_id, amount, status, provider_reference, failure_reason FROM payments;"
```

### Experiments to try

1. **The classic mistake.** In `Pay`, treat the provider error like a decline: call `s.completePayment(ctx, orderID, pay.ID, payments.Result{Status: payments.ResultDeclined, FailureReason: "timeout"})`. Run `TestPay_TimeoutThenRetryChargesOnce`. The order gets cancelled even though the mock *took the money*. That's the "customer paid, got nothing" bug.
2. **Forget the provider key.** In `Pay`, use `IdempotencyKey: fmt.Sprint(time.Now().UnixNano())`. The same test now reports `charges = 2`: a double charge.
3. **Hold locks during the call.** Move the provider call *inside* step 1's transaction. Start the API with `PAYMENT_TIMEOUT=10s`, pay with `TIMEOUT` in one window, and place an order for the same product in another: it waits the full 10 s. Revert.
4. **Global default.** Restart with `MOCK_PAYMENT_OUTCOME=FAILURE`. Every payment without a `simulate` field is now declined.
