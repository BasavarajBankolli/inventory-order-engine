# Inventory & Order Management Engine

A production-style backend in Go that manages products, inventory, and orders.
It is built to handle concurrent orders correctly and **never oversell stock**.

This is a learning and portfolio project, so the code favours clarity over cleverness.
Each module has a matching explanation in [`docs/learning/`](docs/learning/).

> **Status: Stage 12 of 15 — orders, reservations, concurrency, idempotency, payments, worker, Redis, outbox events.**
> This README grows with each stage.

---

## Architecture (so far)

```mermaid
flowchart LR
    client([Client / curl]) -->|HTTP :8080| api
    subgraph docker compose
        migrate[migrate<br/>runs once, exits] -->|applies SQL| pg[(PostgreSQL 17)]
        api[api<br/>Go HTTP server] -->|pgx pool| pg
        worker[worker<br/>expires reservations,<br/>reconciles payments,<br/>publishes outbox events] -->|pgx pool| pg
        api -->|cache + rate limit<br/>optional| redis[(Redis 7)]
    end
```

It is a **modular monolith**: one Go program, split into internal packages with clear
responsibilities. Microservices would add network calls, distributed transactions, and
deployment overhead that this project does not need. Keeping one database also lets us use
real ACID transactions for the "never oversell" guarantee.

### Request pipeline

```mermaid
flowchart LR
    req[HTTP request] --> rid[RequestID] --> log[Logger] --> rec[Recoverer] --> router{chi router}
    router --> h[/health, /ready/]
    router --> pub[/auth/register, /auth/login/]
    router --> pubp[/GET products/]
    router --> ra[RequireAuth] --> me[/users/me/]
    ra --> ord[/orders: create, list, get, cancel/]
    ra --> rr[RequireRole ADMIN] --> adm[/POST, PATCH, DELETE products/]
    rr --> inv[/GET, PATCH products/id/inventory/]
```

Handlers stay thin. Business rules live in services, and SQL lives in repositories:

```text
HTTP handler      ->  service (rules)   ->  repository (SQL)    ->  PostgreSQL
auth.Handler          auth.Service          users.Repository
products.Handler      products.Service      products.Repository (+ inventory.Repository, one transaction)
inventory.Handler     inventory.Service     inventory.Repository   (SELECT ... FOR UPDATE)
orders.Handler        orders.Service        orders.Repository (+ products.Repository, one transaction)
                        └─ inventory.Service.ReserveForOrder / ReleaseForOrder (inside the order's transaction)
```

### Order and reservation flow

```mermaid
sequenceDiagram
    participant C as Customer
    participant O as orders.Service
    participant I as inventory.Service
    participant DB as PostgreSQL
    C->>O: POST /orders
    O->>DB: BEGIN
    O->>DB: load products, INSERT order (CREATED) + items
    O->>I: ReserveForOrder(tx, ...)
    I->>DB: SELECT inventory FOR UPDATE (by product id)
    I->>DB: available -= q, reserved += q, INSERT reservation (ACTIVE, expires_at)
    O->>DB: UPDATE order -> RESERVED
    O->>DB: COMMIT
    O-->>C: 201 RESERVED (or 409 OUT_OF_STOCK after ROLLBACK)
```

### Concurrency: never oversell

| Mechanism | What it prevents |
|---|---|
| One transaction per use case (`database.WithTx`) | Half-finished orders; an order row without its reservation |
| `SELECT ... FOR UPDATE` on the inventory row | Two buyers both "seeing" the last unit (check-then-act race) |
| Locks always taken in product-id order | Deadlocks between multi-item orders |
| `SELECT ... FOR SHARE` on products | A product archived or re-priced in the middle of a checkout |
| `CHECK (available_quantity >= 0)` | A future bug *storing* negative stock |

Proof: `go test -race -count=20 -run Concurrency ./internal/orders/`, plus the live demo:

```powershell
go run ./cmd/concurrency-demo            # 100 buyers, stock 1 -> 1 x 201, 99 x 409 OUT_OF_STOCK
```

> The demo logs in 100+ buyers from one IP, and the rate limiter (100/min) stops that on purpose.
> For demos: `$env:RATE_LIMIT_PER_MINUTE = "5000"; docker compose up -d api`.

Details: [docs/learning/07-transactions-and-concurrency.md](docs/learning/07-transactions-and-concurrency.md).

### Idempotency: retries never duplicate an order

Send `Idempotency-Key: <uuid>` with `POST /orders`. A retry with the same key and body returns the
**original** order (`200 OK` + `Idempotent-Replayed: true`) instead of creating a second one. Even
when duplicates arrive at the same moment, a unique index on `(user_id, idempotency_key)` lets
only one INSERT win.

```powershell
go run ./cmd/concurrency-demo -mode idempotency -buyers 50   # 1 x 201, 49 x 200, one order id
```

Details: [docs/learning/08-idempotency.md](docs/learning/08-idempotency.md).

### Payments (mock provider)

```mermaid
stateDiagram-v2
    [*] --> RESERVED: POST /orders
    RESERVED --> PAYMENT_PENDING: POST /orders/{id}/pay (step 1)
    PAYMENT_PENDING --> CONFIRMED: provider SUCCEEDED (reserved stock sold)
    PAYMENT_PENDING --> PAYMENT_FAILED: provider DECLINED
    PAYMENT_FAILED --> CANCELLED: stock released
    PAYMENT_PENDING --> PAYMENT_PENDING: timeout, outcome unknown (504, retry is safe)
    RESERVED --> CANCELLED: POST /orders/{id}/cancel
```

The provider is called **outside** any transaction (no locks held during the slow call). The
same provider idempotency key is used on every retry, so a timeout followed by a retry never
charges twice. Details: [docs/learning/09-payments.md](docs/learning/09-payments.md).

### Background worker

`cmd/worker` runs every `WORKER_INTERVAL` (10 s) and:

- expires `RESERVED` orders whose reservation ran out (`RESERVATION_TTL`, 15 min), returning their stock;
- resolves `PAYMENT_PENDING` orders stuck after a timeout by **asking the provider** what happened (never guessing): confirmed, cancelled, or expired.

Each order is handled in its own transaction that locks the order and re-checks its state, so
running the worker twice, or two workers at once, is safe. Details:
[docs/learning/10-workers.md](docs/learning/10-workers.md).

### Redis: cache and rate limiting (optional, never the source of truth)

| Feature | How |
|---|---|
| Product cache | Cache-aside on `GET /products/{id}`, 5 min TTL, deleted on update/archive. Orders **never** read it |
| Rate limiting | Fixed window per minute: per IP on public routes, per user when logged in. `429` + `Retry-After` |
| Redis down | Cache → PostgreSQL, rate limiter fails **open**, `/ready` → `degraded`. A circuit breaker skips Redis for 5 s after a failure, so requests stay fast |

Details: [docs/learning/11-redis.md](docs/learning/11-redis.md).

### Events: transactional outbox

`OrderCreated`, `InventoryReserved`, `OrderConfirmed`, `OrderCancelled` and `OrderExpired` are
written to `outbox_events` **in the same transaction** as the change, so an event is never lost
and never describes a change that rolled back. The worker publishes them:

- **In order per order.** An order's later events wait for its earlier ones.
- **Several workers can run at once.** `FOR UPDATE SKIP LOCKED` means each event is published once.
- **Retries** use exponential backoff. After 10 failures an event becomes `FAILED` (dead letter).

Delivery is at-least-once, so consumers must ignore event ids they've already seen. Details:
[docs/learning/12-outbox.md](docs/learning/12-outbox.md).

### Folder layout

```text
cmd/
  api/main.go          Starts the HTTP server (wiring only, no business logic)
  concurrency-demo/    Fires N simultaneous orders at the running API and reports the outcome
  migrate/main.go      Applies pending SQL migrations, then exits
  worker/main.go       Background jobs: reservation expiry and payment reconciliation
internal/
  app/                 Builds every module and connects them (used by main and API tests)
  auth/                Register/login service, bcrypt, JWT, RequireAuth/RequireRole middleware
  cache/               Redis client (fail-fast + circuit breaker) and product cache
  config/              Reads settings from environment variables
  events/              Transactional outbox: write events in the business tx, publish with retries
  database/            PostgreSQL pool, migration runner, WithTx transaction helper
  health/              /health (liveness) and /ready (readiness)
  inventory/           Stock levels: domain rules, row locking, optimistic versioning
  httpx/               Shared JSON helpers: strict body decoding, response and error helpers
  identity/            Principal (user id + role) of the authenticated caller, in context
  logging/             Structured JSON logger (log/slog) that adds request_id automatically
  middleware/          RequestID, Logger, Recoverer
  money/               Money type (integer minor units + currency)
  orders/              Orders + items, price snapshot, totals, state machine, ownership, cancel, pay
  payments/            Provider interface, mock provider, payments repository
  products/            Product catalogue: validation, soft delete, list/search/sort/paginate, cache-aside
  ratelimit/           Fixed-window rate limiter (Redis) + middleware (per IP / per user)
  requestid/           Stores and reads the request ID in context.Context
  server/              Router: maps URLs to handlers
  testutil/            Helpers used only by tests (isolated, migrated test schemas)
  users/               User model, users repository (SQL), GET /users/me
  validate/            Collects per-field validation errors
  worker/              Periodic job runner (errors/panics never stop it)
migrations/            Numbered .sql files, embedded into the binary
docker/postgres/init/  One-time Postgres setup (creates the test database)
docs/learning/         Beginner-friendly explanations for every module
tests/                 End-to-end API tests (HTTP -> router -> services -> PostgreSQL)
```

There is no `pkg/` folder, because
nothing here is meant to be imported by other projects.

## Technology choices

| Need | Choice | Why |
|---|---|---|
| Language | Go 1.26 | Simple, fast, great concurrency and standard library |
| HTTP router | `go-chi/chi/v5` | Tiny, built on `net/http` types, path params like `/products/{id}` |
| PostgreSQL driver | `jackc/pgx/v5` | The most widely used Postgres driver for Go, with a built-in pool |
| Password hashing | `golang.org/x/crypto/bcrypt` | Slow, salted hashing built for passwords; maintained by the Go team |
| Access tokens | `golang-jwt/jwt/v5` | The most widely used Go JWT library; lets us pin the accepted algorithm |
| Logging | `log/slog` (stdlib) | Structured JSON logs with no dependency |
| Config | `os.Getenv` (stdlib) | Env vars are enough; no config library needed |
| Migrations | Own ~100-line runner | Easy to read end to end; uses advisory locks and transactional DDL |
| Database | PostgreSQL 17 | Source of truth: transactions, row locks, constraints |
| Cache / rate limiting | Redis 7 + `redis/go-redis/v9` | Sub-millisecond reads and atomic counters shared by all API instances |



---

## Running locally (Windows PowerShell)

Prerequisites: **Docker Desktop** (running), and **Go 1.26+** if you want to run
the tests or run the binaries outside Docker.

### Option A: everything in Docker

```powershell
cd $HOME\Desktop\inventory-order-engine
docker compose up --build
```

Compose starts `postgres`, waits until it is healthy, runs `migrate` once, then starts `api`.
Press `Ctrl+C` to stop. Add `-d` to run in the background, and use `docker compose logs -f api`
to follow the logs.

The defaults work without any configuration. To change ports or passwords, copy the example
file and edit it:

```powershell
Copy-Item .env.example .env
```

### Option B: database in Docker, Go on your machine (best for debugging)

```powershell
docker compose up -d postgres
$env:DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory?sslmode=disable"
$env:JWT_SECRET = "local-dev-only-jwt-secret-change-me-0123456789"
go run ./cmd/migrate
go run ./cmd/api
```

With this setup you can put breakpoints in VS Code or GoLand and step through requests.

### Migrations

- Files live in `migrations/` and are named `NNNNNN_description.sql`.
- `migrate` applies every file that is not yet in the `schema_migrations` table. Each file runs in its own transaction.
- **Never edit a migration that has already been applied. Add a new file instead.**

```powershell
docker compose run --rm migrate          # in Docker
go run ./cmd/migrate                     # locally (needs $env:DATABASE_URL)
docker compose exec postgres psql -U app -d inventory -c "SELECT * FROM schema_migrations;"
```

### Reset everything (deletes all data)

```powershell
docker compose down -v
```

---

## Testing

```powershell
# Unit tests only (no database needed; integration tests are skipped)
go test ./...

# Unit + integration tests (needs the postgres and redis containers running)
docker compose up -d postgres redis
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
$env:TEST_REDIS_URL = "redis://localhost:6379/15"
go test -count=1 ./...

# Verbose output for one package
go test -count=1 -v ./internal/database/
```

Integration tests use a separate `inventory_test` database, created automatically the first
time the Postgres volume is initialised. Every integration test gets its own temporary schema
(migrated when needed), so tests never see each other's data and can run in parallel.

| Kind | Where | Needs DB |
|---|---|---|
| Unit | `internal/**/..._test.go` (for example `auth/service_test.go`, which uses an in-memory fake store) | no |
| Repository integration | `internal/*/repository_test.go`, `internal/database/migrate_test.go` | yes |
| End-to-end API | `tests/` | yes |

---

## API

All business endpoints are under `/api/v1`. Request and response details are in
[docs/learning/02-authentication.md](docs/learning/02-authentication.md) and
[docs/learning/03-products.md](docs/learning/03-products.md) and
[docs/learning/04-inventory.md](docs/learning/04-inventory.md) and
[docs/learning/05-orders.md](docs/learning/05-orders.md) and
[docs/learning/06-reservations.md](docs/learning/06-reservations.md). A complete API
reference is added in Stage 15.

| Method | URL | Auth | Description | Errors |
|---|---|---|---|---|
| GET | `/health` | none | Liveness: returns 200 if the process is running | – |
| GET | `/ready` | none | Readiness: returns 200 if dependencies are reachable, otherwise 503 | 503 |
| POST | `/api/v1/auth/register` | none | Create a CUSTOMER account | 400 `VALIDATION_ERROR`, 409 `EMAIL_ALREADY_EXISTS` |
| POST | `/api/v1/auth/login` | none | Exchange email and password for a JWT | 400, 401 `INVALID_CREDENTIALS` |
| GET | `/api/v1/users/me` | Bearer | Profile of the logged-in user | 401 `UNAUTHENTICATED` |
| GET | `/api/v1/products` | none | List products: `q`, `status`, `sort`, `limit`, `offset` | 400 |
| GET | `/api/v1/products/{id}` | none | One product (archived products return 404) | 400, 404 |
| POST | `/api/v1/products` | Admin | Create a product | 400, 401, 403, 409 `SKU_ALREADY_EXISTS` |
| PATCH | `/api/v1/products/{id}` | Admin | Partial update (SKU cannot change) | 400, 401, 403, 404 |
| DELETE | `/api/v1/products/{id}` | Admin | Archive (soft delete), returns 204 | 400, 401, 403, 404 |
| GET | `/api/v1/products/{id}/inventory` | Admin | Stock: `available_quantity`, `reserved_quantity`, `version` | 400, 401, 403, 404 |
| POST | `/api/v1/orders` | Bearer | `{"items":[{"product_id":2,"quantity":3}]}` reserves stock and returns a `RESERVED` order (201). Optional `Idempotency-Key` header: a repeat returns the original order (200) | 400, 401, 409 `OUT_OF_STOCK` / `PRODUCT_UNAVAILABLE` / `IDEMPOTENCY_KEY_REUSED` |
| GET | `/api/v1/orders` | Bearer | Your orders (admins see all): `status`, `limit`, `offset` | 400, 401 |
| GET | `/api/v1/orders/{id}` | Bearer | One order with items. Someone else's order returns 404 | 400, 401, 404 |
| POST | `/api/v1/orders/{id}/pay` | Bearer | Pay a `RESERVED` order. Optional body `{"simulate":"SUCCESS"\|"FAILURE"\|"TIMEOUT"}` (mock only). Returns `{order, payment}` | 402 `PAYMENT_FAILED`, 504 `PAYMENT_TIMEOUT`, 409 `RESERVATION_EXPIRED` / `INVALID_STATE_TRANSITION`, 404 |
| POST | `/api/v1/orders/{id}/cancel` | Bearer | Cancel if the state machine allows it; reserved stock goes back to available | 404, 409 `INVALID_STATE_TRANSITION` |
| PATCH | `/api/v1/products/{id}/inventory` | Admin | `{"adjustment": 25}` or `{"available_quantity": 100, "version": 3}` | 400, 404, 409 `INSUFFICIENT_STOCK` / `VERSION_CONFLICT` |

**Money:** `price` is an integer in **minor units** (paise or cents). `"price": 129900, "currency": "INR"`
means ₹1,299.00. Floats are never used for money.

Every response has an `X-Request-ID` header. Every error uses this shape (`fields` appears only
on validation errors):

```json
{ "error": { "code": "VALIDATION_ERROR", "message": "one or more fields are invalid",
             "fields": { "email": "must be a valid email address" }, "request_id": "..." } }
```

### Try it (PowerShell)

```powershell
$base = "http://localhost:8080/api/v1"

# 1. Register
$body = @{ email = "alice@example.com"; name = "Alice"; password = "super-secret-1" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "$base/auth/register" -ContentType "application/json" -Body $body

# 2. Log in and keep the token
$login = Invoke-RestMethod -Method Post -Uri "$base/auth/login" -ContentType "application/json" `
  -Body (@{ email = "alice@example.com"; password = "super-secret-1" } | ConvertTo-Json)
$headers = @{ Authorization = "Bearer $($login.access_token)" }

# 3. Call a protected endpoint
Invoke-RestMethod -Uri "$base/users/me" -Headers $headers

# See an error body (Invoke-RestMethod throws on 4xx, so catch it)
try { Invoke-RestMethod -Uri "$base/users/me" } catch { $_.ErrorDetails.Message }

# 4. Products (the create call needs an ADMIN token; see "Creating an admin" below)
$p = @{ sku = "MOUSE-01"; name = "Wireless Mouse"; price = 129900; currency = "INR" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "$base/products" -Headers $headers -ContentType "application/json" -Body $p
Invoke-RestMethod -Uri "$base/products?q=mouse&sort=price_asc&limit=10"
Invoke-RestMethod -Method Patch -Uri "$base/products/1" -Headers $headers -ContentType "application/json" -Body '{"price": 99900}'
Invoke-WebRequest -Method Delete -Uri "$base/products/1" -Headers $headers -UseBasicParsing   # 204

# 5. Inventory (admin): every product starts with 0 available
Invoke-RestMethod -Uri "$base/products/2/inventory" -Headers $headers
Invoke-RestMethod -Method Patch -Uri "$base/products/2/inventory" -Headers $headers -ContentType "application/json" -Body '{"adjustment": 50}'
Invoke-RestMethod -Method Patch -Uri "$base/products/2/inventory" -Headers $headers -ContentType "application/json" -Body '{"available_quantity": 45, "version": 2}'

# 6. Orders (any logged-in user)
$order = Invoke-RestMethod -Method Post -Uri "$base/orders" -Headers $headers -ContentType "application/json" `
  -Body '{"items":[{"product_id":2,"quantity":2}]}'
Invoke-RestMethod -Uri "$base/orders" -Headers $headers
Invoke-RestMethod -Method Post -Uri "$base/orders/$($order.id)/cancel" -Headers $headers
```

> In Windows PowerShell, `curl` is an alias for `Invoke-WebRequest`. If you use real curl, type
> `curl.exe`. Passing JSON to `curl.exe` from PowerShell 5.1 needs escaped quotes, which is why
> the examples use `Invoke-RestMethod`.

### Creating an admin

Public registration always creates a `CUSTOMER`. An operator promotes a user directly in the
database, and the user then **logs in again**. The role is stored inside the token, so older
tokens keep the old role until they expire.

```powershell
docker compose exec postgres psql -U app -d inventory -c "UPDATE users SET role = 'ADMIN', updated_at = now() WHERE email = 'alice@example.com';"
```

---

## Roadmap

1. ✅ Project setup, Docker, PostgreSQL
2. ✅ Authentication (register, login, JWT, roles)
3. ✅ Products
4. ✅ Inventory
5. ✅ Orders
6. ✅ Inventory reservations
7. ✅ Transactions and concurrency (100 concurrent buyers, 1 item in stock)
8. ✅ Idempotency keys
9. ✅ Mock payments
10. ✅ Background workers (reservation expiry)
11. ✅ Redis (cache and rate limiting)
12. ✅ Transactional outbox
13. Test suite hardening
14. Observability (Prometheus metrics)
15. Deployment and full documentation
