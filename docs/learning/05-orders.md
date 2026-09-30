# 05 — Orders & the Order State Machine

> **Stage 5 scope.** Orders are created, priced, stored and cancelled, but they **do not touch
> stock yet**. Reserving inventory is Stage 6, and the "100 buyers, 1 item" guarantee is
> Stage 7. So right now two customers can both order the last unit. That's expected at this
> stage, and it's exactly what the next two stages fix.

## What problem does this solve?

A customer wants to buy several products in one go. The system has to:

1. Record **what** was bought, **how many**, and **at what price**. The price must be frozen at the moment of ordering.
2. Calculate the **total** exactly.
3. Track the order through a **lifecycle** (created → reserved → paid → shipped …) and refuse nonsense such as "delivered → created".
4. Make sure customers only ever see and change **their own** orders.

## How does our implementation work?

```text
orders.Handler     JSON <-> Go, caller from context                  (handler.go)
orders.Service     transactions, ownership rules                     (service.go)
validateItems      request shape: 1-50 lines, qty 1-1000, no dupes   (order.go)
buildItems         price snapshot + totals + product checks, pure Go (order.go)
transitions map    THE state machine                                 (status.go)
orders.Repository  SQL for orders + order_items                      (repository.go)
```

### Two tables: `orders` and `order_items`

```text
orders                                   order_items
id  user_id  status   total  currency    order_id  product_id  qty  unit_price  total_price
1   3        CREATED  709700 INR         1         1           1    449900      449900
                                         1         2           2    129900      259800
```

Why a separate items table? An order has a *variable* number of lines. One row per line keeps
every column simple and lets the database enforce rules per line:
- `quantity > 0`
- `total_price = unit_price * quantity`
- the same product can't appear twice in one order

### Price snapshot

`order_items.unit_price` is **copied** from `products.price` when the order is created. If the
admin raises the price tomorrow, yesterday's order still says what the customer actually agreed
to pay. You saw this in the manual test: the mouse price changed to 199900, and the order kept
129900.

### Total calculation (`buildItems`)

```text
line total  = unit_price × quantity           129900 × 2 = 259800
order total = Σ line totals                   449900 + 259800 = 709700
```

All the numbers are `int64` minor units. Overflow can't happen because the limits cap the worst
case: 50 lines × 1,000 units × 1,000,000,000 max price = 5×10¹³, far below int64's 9.2×10¹⁸.
`TestBuildItems_LargestPossibleOrderDoesNotOverflow` checks exactly that worst case.

`buildItems` is a **pure function**: it takes requested lines and already-loaded products, and
returns items and a total. That's why the core money logic can be unit-tested without a database.

### The state machine (`status.go`)

```text
CREATED ──► RESERVED ──► PAYMENT_PENDING ──► CONFIRMED ──► PROCESSING ──► SHIPPED ──► DELIVERED
   │           │  │            │   │
   │           │  └─► EXPIRED ◄┘   └─► PAYMENT_FAILED ──► CANCELLED
   └───────────┴──────────────────────────────────────────► CANCELLED
```

```go
var transitions = map[Status][]Status{
    StatusCreated:        {StatusReserved, StatusCancelled},
    StatusReserved:       {StatusPaymentPending, StatusCancelled, StatusExpired},
    StatusPaymentPending: {StatusConfirmed, StatusPaymentFailed, StatusExpired},
    ...
}
```

- The rules are **data**, kept in one map. Reviewing the lifecycle means reading ten lines.
- `order.TransitionTo(next)` is the **only** way to change a status. It returns `ErrInvalidTransition` and leaves the order untouched when the move isn't allowed.
- `DELIVERED`, `CANCELLED` and `EXPIRED` are **final**: they have no outgoing transitions.
- `PAYMENT_PENDING → CANCELLED` is deliberately **missing**. If a customer could cancel while the charge is in flight, the charge might succeed a moment later. The money is taken, but the order is cancelled.
- The API never accepts a status from the client. There's no `PATCH {"status": ...}`, only the specific action `POST /orders/{id}/cancel`. Sending `"status"` in the create body is rejected as an unknown field.

`TestCanTransition_AllPairs` checks **all 100** (from, to) combinations against a separately
written list of allowed moves. If you add or remove a transition by accident, the test fails.

### Ownership: 404, not 403

For Bob, Alice's order returns **404 Not Found**, not 403 Forbidden. A 403 would confirm that
order #123 exists, which leaks information: competitors could count your orders by probing ids.
Admins can see and cancel every order. The rule lives in `canAccess` in the **service**. A new
handler can't forget it.

## What happens during a normal request?

`POST /api/v1/orders {"items":[{"product_id":1,"quantity":1},{"product_id":2,"quantity":2}]}` from Bob:

1. `RequireAuth` puts `Principal{UserID: 3, Role: CUSTOMER}` in the context.
2. `DecodeJSON` rejects unknown fields (`status`, `unit_price` …): clients can't choose prices or statuses.
3. `validateItems` checks there are 1–50 lines, each quantity is 1–1000, and no product appears twice.
4. `BEGIN`
5. `SELECT ... FROM products WHERE id = ANY($1)` loads current prices and statuses **inside** the transaction.
6. `buildItems` checks the products: archived or missing → 400; inactive → 409; mixed currencies → 400. Then it builds the price snapshot and total.
7. `INSERT INTO orders ... RETURNING`, followed by one `INSERT INTO order_items` per line.
8. `COMMIT`
9. The service logs `"order created" order_id=1 total_amount=709700 items=2 user_id=3 request_id=...`.
10. The response is `201`, with `Location: /api/v1/orders/1` and the order including its items.

`POST /api/v1/orders/1/cancel`:

1. `BEGIN`, then `SELECT ... FROM orders WHERE id = 1 FOR UPDATE` locks the order row.
2. `canAccess` runs: someone else's order → 404.
3. `TransitionTo(CANCELLED)`: not allowed from this status → 409 `INVALID_STATE_TRANSITION`.
4. `UPDATE orders SET status = 'CANCELLED' WHERE id = 1 AND status = 'CREATED'`, then `COMMIT`.

## What happens when it fails?

| Situation | Result | Rows written |
|---|---|---|
| No items, qty 0 or > 1000, duplicate product line | 400 `VALIDATION_ERROR` with `items[i].field` | none, rejected before BEGIN |
| Unknown or archived product | 400, e.g. `items[1].product_id: product 3 does not exist` | none, ROLLBACK |
| Inactive product | 409 `PRODUCT_UNAVAILABLE` | none, ROLLBACK |
| Products in different currencies | 400 `items: all products in one order must use the same currency` | none, ROLLBACK |
| `status`, `unit_price`… in the body | 400 unknown field | none |
| Someone else's order (GET or cancel) | 404 | none |
| Cancel a CANCELLED, PAYMENT_PENDING or SHIPPED… order | 409 `INVALID_STATE_TRANSITION` | none, ROLLBACK |
| Database error while inserting item 2 of 3 | 500 | none: the order row and item 1 are rolled back too |

`TestCreate_RejectsAndWritesNothing` checks the "rows written: none" column: after four
rejected orders, `orders` and `order_items` are both empty.

## What database concepts are involved?

| Concept | Where |
|---|---|
| One-to-many relationship (`order_items.order_id → orders.id`) | migration 000005 |
| Foreign keys to `users` and `products` | Orders can't point at users or products that don't exist, and those rows can't be hard-deleted |
| `CHECK (total_price = unit_price * quantity)` | A rule that relates **columns of the same row** |
| `UNIQUE (order_id, product_id)` | One line per product. Its index also serves "items of order X" |
| `INDEX (user_id, id DESC)` | "My orders, newest first" reads rows already in order |
| `WHERE id = ANY($1)` | One query and one parameter (an array) for any number of ids |
| `($1 = 0 OR user_id = $1)` | One fixed query serves "my orders" and "all orders" |
| Transactions (`database.WithTx`) | The order and its items appear together or not at all |
| `SELECT ... FOR UPDATE` on the order row | Serialises concurrent status changes on the same order |
| Compare-and-set `UPDATE ... WHERE status = $old` | Second safety net: never overwrite a status you didn't see |

A cross-table rule such as "orders.total_amount = sum of its items" **can't** be a CHECK
constraint, because CHECK only sees one row. The service guarantees it by computing both in
`buildItems` and writing them in the same transaction.

## What concurrency issues exist?

- **Double cancel.** Twenty simultaneous `cancel` calls for one order: the `FOR UPDATE` lock makes them take turns. The first moves CREATED → CANCELLED, and the other 19 see CANCELLED and get `INVALID_STATE_TRANSITION`. `TestCancel_Concurrent` proves exactly 1 success and 19 conflicts. The same lock stops "cancel" from racing with "payment succeeded" in Stage 9.
- **Price changes during checkout.** Products are read inside the order transaction, so the snapshot is the price at that moment. (A product row isn't locked, so an admin could change the price a millisecond after we read it. The order still has a consistent price: the one we read and showed in the response.)
- **Overselling.** It isn't handled yet: orders don't touch inventory in Stage 5. Stage 6 adds the reservation inside this same transaction, and Stage 7 makes it correct under 100 concurrent buyers.

## How can I reproduce/test it?

```powershell
# Unit: state machine (all 100 pairs), totals, validation
go test ./internal/orders/ -run "Transition|IsFinal|BuildItems|ValidateItems" -v

# Integration + API
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 ./internal/orders/ ./internal/database/ ./tests/ -run "Create|Ownership|Cancel|List|Orders|WithTx" -v
```

Manually (with `$bob` as a customer token and `$admin` as an admin token; see the README):

```powershell
$base = "http://localhost:8080/api/v1"
$o = Invoke-RestMethod -Method Post -Uri "$base/orders" -Headers $bob -ContentType "application/json" `
  -Body '{"items":[{"product_id":1,"quantity":1},{"product_id":2,"quantity":2}]}'
Invoke-RestMethod -Uri "$base/orders/$($o.id)" -Headers $bob
Invoke-RestMethod -Method Post -Uri "$base/orders/$($o.id)/cancel" -Headers $bob
docker compose exec postgres psql -U app -d inventory -c "SELECT * FROM orders; SELECT * FROM order_items;"
```

### Experiments to try

1. **Break the state machine.** Add `StatusCreated` to the list for `StatusDelivered` in `transitions`, then run `go test ./internal/orders/ -run AllPairs`. The test names the exact pair you allowed.
2. **Prove the snapshot.** Create an order, PATCH the product's price, and GET the order again. The unit price is unchanged.
3. **See the rollback.** In `Repository.Create`, change the loop header to `for i, it := range o.Items {`, and at the end of the loop body add `if i == 0 { return Order{}, errors.New("boom") }`. Place a 2-item order: you get a 500, and `SELECT count(*) FROM orders` hasn't changed, even though the order row and the first item were already inserted. Revert the change afterwards.
4. **Try to cheat.** Send `{"items":[{"product_id":1,"quantity":1,"unit_price":1}]}`. You get a 400, because the client can't set prices.
5. **Watch the lock.** In psql window 1, run `BEGIN; SELECT * FROM orders WHERE id = 1 FOR UPDATE;`. Then call the cancel endpoint for order 1: it hangs until you `COMMIT` in psql.
