# Inventory & Order Management Engine

A production-style backend in **Go** for products, inventory and orders that **never oversells
stock**, even when 100 customers try to buy the last item at the same moment.

It covers the problems real commerce backends face: transactions and row locking, idempotent
retries, reservations that expire, payments that time out, events that must never be lost, a
cache that must never lie, and rate limiting behind a load balancer. All of it is tested (396
tests, 89.7 % coverage, race detector on) and deployable for free.

This is a learning and portfolio project, so the code favours **clarity over cleverness**, and
every module has a beginner-friendly explanation in [`docs/learning/`](docs/learning/).

| | |
|---|---|
| **API reference** | [docs/API.md](docs/API.md): every endpoint, with tested curl examples |
| **Deployment** | [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md): Render (API, PostgreSQL, Redis) + Vercel (frontend) |
| **Frontend** | [frontend/](frontend/README.md): React + TypeScript shop and admin panel |
| **Learning notes** | [docs/learning/01 … 15](docs/learning/): one per module, plus interview questions |

---

## Contents

1. [Project overview](#project-overview)
2. [Architecture](#architecture) · [Why a modular monolith](#why-a-modular-monolith)
3. [Technology choices](#technology-choices)
4. [Database design](#database-design)
5. [Order flow](#order-flow) · [Inventory reservation flow](#inventory-reservation-flow)
6. [Concurrency handling](#concurrency-handling) · [Idempotency](#idempotency)
7. [Redis usage](#redis-usage) · [Background workers](#background-workers) · [Outbox pattern](#outbox-pattern)
8. [Failure handling](#failure-handling) · [Observability](#observability)
9. [Local setup](#local-setup) · [Docker setup](#docker-setup) · [Walkthrough: from zero to a paid order](#walkthrough-from-zero-to-a-paid-order)
10. [The three demonstrations](#the-three-demonstrations)
11. [Testing](#testing)
12. [API documentation](#api-documentation) · [Deployment](#deployment)
13. [Future improvements](#future-improvements)

---

## Project overview

| Feature | What it does |
|---|---|
| Auth | Register and log in (bcrypt passwords, JWT access tokens), roles `CUSTOMER` / `ADMIN` |
| Products | Admin CRUD with soft delete; public list with search, sort and pagination |
| Inventory | `available` / `reserved` stock per product; adjustments under row locks; optimistic `version` for absolute updates |
| Orders | Multi-item orders, price snapshot, state machine, ownership checks |
| Reservations | Stock is held for 15 min per order line; released on cancel, payment failure or expiry |
| Payments | Mock provider with success, decline and timeout; a retry never charges twice |
| Idempotency | `Idempotency-Key` header: retries return the original order |
| Worker | Expires reservations, reconciles stuck payments, publishes outbox events |
| Redis | Product cache (cache-aside) and per-user / per-IP rate limiting; optional, fails safe |
| Outbox | Domain events written in the same transaction as the change; published with retries |
| Observability | JSON logs with request ids, Prometheus metrics, liveness and readiness probes |

**The guarantee everything is built around:** stock is never oversold, never negative, and
never "lost" in a reservation that nobody will release.

---

## Architecture

```mermaid
flowchart LR
    browser([React frontend<br/>Vercel]) -->|HTTPS, JWT| api
    curl([curl / Postman]) --> api
    subgraph backend [docker compose / Render]
        migrate[migrate<br/>applies SQL, exits] --> pg[(PostgreSQL 17<br/>source of truth)]
        api[api<br/>chi HTTP server] -->|pgx pool, transactions| pg
        api -->|cache + rate limit<br/>optional| redis[(Redis 7)]
        worker[worker<br/>expiry, payment reconciliation,<br/>outbox publisher] -->|pgx pool| pg
    end
    prom([Prometheus]) -.->|/metrics| api
    prom -.->|:9091/metrics| worker
```

Inside the API, every request passes through the same pipeline. Business rules live in
**services**, SQL lives in **repositories**, and handlers only translate HTTP:

```mermaid
flowchart LR
    req[request] --> cors[CORS] --> rid[RequestID] --> log[Logger + metrics] --> rec[Recoverer]
    rec --> pub[public routes<br/>rate limit per IP]
    rec --> auth[RequireAuth<br/>JWT] --> rlu[rate limit per user] --> cust[orders, users/me]
    rlu --> admin[RequireRole ADMIN] --> adm[products write, inventory]
```

```text
HTTP handler      ->  service (rules)   ->  repository (SQL)       ->  PostgreSQL
orders.Handler        orders.Service        orders.Repository
                        ├─ inventory.Service.ReserveForOrder / ReleaseForOrder   (same transaction)
                        ├─ payments.Provider (mock)                               (outside any transaction)
                        └─ events.Outbox.Add                                      (same transaction)
```

### Why a modular monolith

One Go program, split into packages with clear boundaries (`orders`, `inventory`, `payments`,
…), deployed as one API process plus one worker process.

- **Real ACID transactions.** "Create the order, reserve the stock, record the event" either
  all happens or none of it does: one `BEGIN … COMMIT` in one database. With microservices this
  becomes a distributed saga with compensations, which is far harder to get right.
- **No network calls between modules.** No partial failures, retries or versioned contracts
  between our own components.
- **Simple to run, debug and deploy.** One `docker compose up`, one debugger session, one
  deploy.
- **Boundaries are still enforced.** Modules talk through small interfaces (e.g.
  `payments.Provider`), so a module could be extracted later if it ever needed independent
  scaling. That decision is easy to make later and expensive to undo.

### Folder layout

```text
cmd/
  api/                 HTTP server (wiring only: config -> pool -> app -> serve -> graceful shutdown)
  worker/              Background jobs + /metrics and /health on :9091
  migrate/             Applies pending SQL migrations, then exits
  concurrency-demo/    Live demos: 100 buyers vs 1 item, idempotent retries, payment failure
internal/
  app/                 Builds every module and connects them (used by main and by API tests)
  auth/                Register/login, bcrypt, JWT, RequireAuth / RequireRole middleware
  cache/               Redis client (fail-fast + circuit breaker) and product cache
  config/              Settings from environment variables, validated at startup
  database/            pgx pool, migration runner (advisory lock), WithTx helper
  events/              Transactional outbox: write in the business tx, publish with retries
  health/              /health (liveness) and /ready (readiness)
  httpx/               Strict JSON decoding, response and error helpers, error codes
  identity/            The authenticated caller (user id + role) in the request context
  inventory/           Stock levels and reservations: row locks, optimistic versioning
  logging/             JSON logger (log/slog) that adds request_id automatically
  metrics/             Prometheus metrics (own registry, nil-safe)
  middleware/          RequestID, Logger, Recoverer, CORS, static bearer token
  money/               Integer minor units + currency
  orders/              Orders, state machine, create/pay/cancel, expiry and reconciliation
  payments/            Provider interface, mock provider, payments repository
  products/            Catalogue: validation, soft delete, search/sort/paginate, cache-aside
  ratelimit/           Fixed-window limiter (Redis), client IP behind proxies
  requestid/, users/, validate/, worker/, server/ (router), testutil/ (test helpers)
migrations/            Numbered .sql files, embedded into the binary
tests/                 End-to-end API tests (HTTP -> router -> services -> PostgreSQL)
scripts/               test.ps1 (whole test suite), demo.ps1 (the three demonstrations)
docs/                  API.md, DEPLOYMENT.md, learning/ (one explanation per module)
frontend/              React 19 + TypeScript + Vite, deployable to Vercel
render.yaml            Render Blueprint (infrastructure as code)
```

There is no `pkg/` folder: nothing here is meant to be imported by other projects, and
`internal/` makes the compiler enforce that.

---

## Technology choices

| Need | Choice | Why |
|---|---|---|
| Language | Go 1.26 | Simple, fast, great concurrency primitives and standard library |
| HTTP router | `go-chi/chi/v5` | Tiny, uses standard `net/http` types, route patterns like `/orders/{id}` |
| PostgreSQL driver | `jackc/pgx/v5` | The most widely used Go Postgres driver, with a built-in pool |
| Database | PostgreSQL 17 | Transactions, row locks, `SKIP LOCKED`, constraints, advisory locks |
| Cache / rate limits | Redis 7 + `redis/go-redis/v9` | Sub-millisecond reads, atomic counters shared by all API instances |
| Passwords | `golang.org/x/crypto/bcrypt` | Slow, salted, built for passwords |
| Tokens | `golang-jwt/jwt/v5` | Widely used; we pin the accepted algorithm (HS256) |
| Logging | `log/slog` (stdlib) | Structured JSON, no dependency |
| Metrics | `prometheus/client_golang` | The standard for Prometheus |
| Migrations | Own ~140-line runner | Readable end to end: embedded SQL, advisory lock, one transaction per file |
| Config | `os.Getenv` | Environment variables are enough (12-factor) |
| Containers | Docker multi-stage build, Docker Compose | Same image locally and in production; non-root user |

---

## Database design

PostgreSQL is the **only source of truth**. Redis holds nothing that can't be rebuilt.

```mermaid
erDiagram
    users ||--o{ orders : places
    products ||--|| inventory : "stock for"
    orders ||--|{ order_items : contains
    products ||--o{ order_items : "sold as"
    orders ||--|{ inventory_reservations : holds
    products ||--o{ inventory_reservations : "reserved from"
    orders ||--o| payments : "paid by"

    users { bigint id PK
            citext email UK
            text password_hash
            text role "CUSTOMER | ADMIN" }
    products { bigint id PK
               text sku UK
               bigint price "minor units"
               text currency
               text status "ACTIVE | INACTIVE | ARCHIVED" }
    inventory { bigint product_id FK
                int available_quantity ">= 0"
                int reserved_quantity ">= 0"
                bigint version }
    orders { bigint id PK
             bigint user_id FK
             text status
             bigint total_amount
             text idempotency_key "unique per user"
             text request_hash }
    order_items { bigint order_id FK
                  bigint product_id FK
                  int quantity
                  bigint unit_price "snapshot"
                  bigint total_price }
    inventory_reservations { bigint order_id FK
                             bigint product_id FK
                             int quantity
                             text status "ACTIVE | CONFIRMED | RELEASED | EXPIRED"
                             timestamptz expires_at }
    payments { bigint order_id FK
               bigint amount
               text status "PENDING | SUCCEEDED | FAILED"
               text provider_reference }
    outbox_events { bigint id PK
                    text event_type
                    bigint aggregate_id
                    jsonb payload
                    text status "PENDING | PROCESSED | FAILED"
                    int attempts }
```

Design rules, enforced **by the database**, not only by Go code:

- **Money is `BIGINT` minor units.** `129900` + `INR` = ₹1,299.00. Floats never touch money.
- **Invariants are `CHECK` constraints.** `available_quantity >= 0`,
  `total_price = unit_price * quantity`, a `SUCCEEDED` payment must have a provider reference, …
  A future bug can't *store* an impossible state.
- **Uniqueness is a constraint, not a `SELECT` first.** Emails (`CITEXT`, case-insensitive),
  SKUs, one inventory row per product, one payment per order, and
  `(user_id, idempotency_key)` as a partial unique index. Constraints stay correct under
  concurrency; "check, then insert" does not.
- **Soft delete for products** (`ARCHIVED`), because order history must keep pointing at them.
- **Partial indexes for the worker's queries**, e.g. active reservations by `expires_at`, and
  pending outbox events by `next_attempt_at`.
- `mock_provider_charges` stores the fake payment provider's state, so it survives restarts like
  a real provider would.

Migrations: `migrations/000001 … 000010`, applied in order, each in its own transaction. Never
edit an applied migration; add a new one.

---

## Order flow

```mermaid
stateDiagram-v2
    [*] --> RESERVED: POST /orders (stock reserved)
    RESERVED --> PAYMENT_PENDING: POST /orders/{id}/pay
    PAYMENT_PENDING --> CONFIRMED: payment succeeded (reserved stock sold)
    PAYMENT_PENDING --> PAYMENT_FAILED: declined
    PAYMENT_FAILED --> CANCELLED: stock released
    RESERVED --> CANCELLED: POST /orders/{id}/cancel (stock released)
    RESERVED --> EXPIRED: reservation TTL passed (worker, stock released)
    PAYMENT_PENDING --> EXPIRED: provider never charged (worker)
    CONFIRMED --> [*]
```

Transitions are a **map in one place** (`orders/status.go`). Every status change goes through
`TransitionTo`, so an illegal move (e.g. cancelling a paid order) is a `409
INVALID_STATE_TRANSITION`, never a silent update.

**Paying** is a three-step flow, so no database lock is held while waiting for the slow,
unreliable provider:

1. **Short transaction:** lock the order, `RESERVED → PAYMENT_PENDING`, insert a `PENDING`
   payment, commit.
2. **No transaction:** call the provider with idempotency key `payment-<payment id>`, the same on every retry.
3. **Short transaction:** record the result. On success, confirm the reservations
   (`reserved -= q`, so the stock is sold). On decline, release them (`available += q`). On
   timeout, change nothing: the outcome is unknown, so we never guess.

Details: [docs/learning/05-orders.md](docs/learning/05-orders.md),
[09-payments.md](docs/learning/09-payments.md).

## Inventory reservation flow

```mermaid
sequenceDiagram
    participant C as Customer
    participant O as orders.Service
    participant I as inventory.Service
    participant DB as PostgreSQL
    C->>O: POST /orders {items}
    O->>DB: BEGIN
    O->>DB: SELECT products ... FOR SHARE (no re-price/archive mid-checkout)
    O->>DB: INSERT order + items (price snapshot)
    O->>I: ReserveForOrder(tx, lines sorted by product id)
    loop each line
        I->>DB: SELECT inventory ... FOR UPDATE
        I->>DB: available -= q, reserved += q
        I->>DB: INSERT reservation ACTIVE, expires_at = now() + 15 min
    end
    O->>DB: order -> RESERVED, INSERT outbox events
    O->>DB: COMMIT
    O-->>C: 201 RESERVED
    Note over O,DB: Not enough stock on any line -> ROLLBACK everything -> 409 OUT_OF_STOCK
```

A reservation ends exactly once (`UPDATE … WHERE status = 'ACTIVE'`):

| Event | Reservation | Inventory |
|---|---|---|
| Payment succeeded | `CONFIRMED` | `reserved -= q` (sold) |
| Cancel / payment declined | `RELEASED` | `reserved -= q`, `available += q` |
| 15 min passed without payment | `EXPIRED` (worker) | `reserved -= q`, `available += q` |

Details: [docs/learning/06-reservations.md](docs/learning/06-reservations.md).

---

## Concurrency handling

| Mechanism | What it prevents |
|---|---|
| One transaction per use case (`database.WithTx`) | Half-finished work, e.g. an order without its reservation |
| `SELECT … FOR UPDATE` on the inventory row | Two buyers both "seeing" the last unit (check-then-act race) |
| Locks always taken in product-id order | Deadlocks between multi-item orders |
| `SELECT … FOR SHARE` on products | A product re-priced or archived in the middle of a checkout |
| Optimistic `version` on absolute stock updates | An admin silently overwriting another admin's change (`409 VERSION_CONFLICT`) |
| `CHECK (available_quantity >= 0)` | A future bug storing negative stock |
| `FOR UPDATE SKIP LOCKED` in the outbox | Two workers publishing the same event |
| Advisory lock in the migration runner | Two instances migrating at once |

Proof: concurrency tests start N goroutines behind a "starting gun" channel, so they really
collide, and run under the race detector. There's also a live demo (see
[The three demonstrations](#the-three-demonstrations)).
Details: [docs/learning/07-transactions-and-concurrency.md](docs/learning/07-transactions-and-concurrency.md).

## Idempotency

Networks fail *after* the server did the work: the response is lost, and the client retries.
Without protection, that creates a second order.

- The client sends `Idempotency-Key: <uuid>` with `POST /orders`.
- The key and a SHA-256 **fingerprint of the request body** are stored on the order, under a
  unique index on `(user_id, idempotency_key)`.
- Same key + same body → `200 OK`, `Idempotent-Replayed: true`, the **original** order.
- Same key + different body → `409 IDEMPOTENCY_KEY_REUSED` (a client bug, so it's surfaced).
- Two identical requests **at the same instant**: the unique index lets exactly one `INSERT`
  win. The loser's transaction rolls back (releasing its reservation) and it returns the winner's
  order.

Payments use the same idea towards the provider (key `payment-<payment id>`), so a retry after a timeout
asks "what happened to this charge?" instead of charging again.
Details: [docs/learning/08-idempotency.md](docs/learning/08-idempotency.md).

## Redis usage

Redis is **optional and never the source of truth**. The API runs without it.

| Feature | How | If Redis is down |
|---|---|---|
| Product cache | Cache-aside on `GET /products/{id}`, 5 min TTL, deleted on update/archive. **Orders and stock never read the cache** | Read from PostgreSQL |
| Rate limiting | Fixed window per minute (`INCR` + `EXPIRE`): per IP on public routes, per user on authenticated ones. `429` + `Retry-After` | **Fail open**: requests are allowed |
| Circuit breaker | After a Redis failure, skip Redis for 5 s | Requests stay fast instead of waiting for timeouts |

`/ready` reports `degraded` (still 200) when Redis is down.
Details: [docs/learning/11-redis.md](docs/learning/11-redis.md).

## Background workers

`cmd/worker` runs every `WORKER_INTERVAL` (10 s):

| Job | What it does |
|---|---|
| `expire-reservations` | `RESERVED` orders past `expires_at` → `EXPIRED`, stock returned |
| (same job) payment reconciliation | `PAYMENT_PENDING` orders stuck after a timeout: **ask the provider** (read-only) what happened → `CONFIRMED`, `CANCELLED` or `EXPIRED`. Never guess |
| `publish-outbox` | Publish pending events (below) |

Each order is handled in its own small transaction that locks the order and re-checks its
state, so running the worker twice, or two workers at once, is safe. A failing or panicking job
is logged and retried on the next tick; it never stops the process.
Details: [docs/learning/10-workers.md](docs/learning/10-workers.md).

## Outbox pattern

Problem: "commit to PostgreSQL, then publish to a message broker" can lose the event (crash in
between), and "publish, then commit" can announce a change that rolled back.

Solution: `OrderCreated`, `InventoryReserved`, `OrderConfirmed`, `OrderCancelled` and
`OrderExpired` are **inserted into `outbox_events` in the same transaction** as the change.
The worker then publishes them:

- **Never lost, never phantom.** The event commits or rolls back with the change.
- **In order per order.** An order's later events wait for its earlier ones.
- **Several workers can run.** `FOR UPDATE SKIP LOCKED` hands each event to one worker.
- **Retries with exponential backoff.** After 10 failures the event becomes `FAILED`
  (dead letter) for a human to inspect.
- **At-least-once delivery.** Consumers de-duplicate by event id.

The publisher logs events today. Swapping in Kafka or SNS is one interface.
Details: [docs/learning/12-outbox.md](docs/learning/12-outbox.md).

---

## Failure handling

| Failure | What happens |
|---|---|
| Two buyers, one unit | Row lock: one gets `201`, the other `409 OUT_OF_STOCK` |
| Client retries an order (timeout, double-click) | Same `Idempotency-Key` → original order, no duplicate |
| Payment declined | `402`; order `CANCELLED`; reservation released; stock back |
| Payment provider times out | `504`; order stays `PAYMENT_PENDING`; retry is safe (no double charge); the worker reconciles if nobody retries |
| Customer never pays | Worker expires the reservation after 15 min; stock back; paying later → `409 RESERVATION_EXPIRED` |
| PostgreSQL down | `/ready` → 503; requests fail fast with `500 INTERNAL_ERROR` (details only in logs); the worker logs and retries next tick |
| Redis down | Cache bypassed, rate limiter fails open, `/ready` → `degraded`; everything else works |
| Client disconnects mid-request | The request context is cancelled, and pgx cancels the running query on the server |
| Panic in a handler | Recoverer returns `500` with the request id; the stack trace goes to logs only |
| Outbox publish fails | Retried with backoff, then dead-lettered; the business transaction was already safe |
| Deploy / `SIGTERM` | Graceful shutdown: in-flight requests and the current worker job finish |

Every failure above has an automated test. See [Testing](#testing).

## Observability

- **Logs:** one JSON line per request with `request_id`, `route`, `status`, `duration_ms` and
  `user_id`. Never query strings, headers or bodies.
- **Metrics** (`GET /metrics`, worker `:9091/metrics`): request rate, latency histogram,
  `orders_created_total`, `orders_failed_total{reason}`, `payments_*`,
  `inventory_reservations_total{event}`, `outbox_events_processed_total{result}`, cache hits.
  Business metrics count **committed** work exactly once.
- **Health:** `/health` (liveness: the process is up), `/ready` (readiness: PostgreSQL
  reachable).

Details: [docs/learning/14-observability.md](docs/learning/14-observability.md).

---

## Local setup

Prerequisites: **Docker Desktop** (running). For tests and debugging also **Go 1.26+**; for the
frontend, **Node.js 20+**.

### Option A: everything in Docker (recommended)

See [Docker setup](#docker-setup) below.

### Option B: databases in Docker, Go on your machine (best for debugging)

```powershell
docker compose up -d postgres redis
$env:DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory?sslmode=disable"
$env:REDIS_URL    = "redis://localhost:6379/0"
$env:JWT_SECRET   = "local-dev-only-jwt-secret-change-me-0123456789"
go run ./cmd/migrate
go run ./cmd/api          # in another terminal (same env vars): go run ./cmd/worker
```

Now you can set breakpoints in VS Code or GoLand and step through a request.

### Configuration

Every setting is an environment variable, validated at startup. All of them are documented in
[`.env.example`](.env.example). The defaults work out of the box locally; copy the file to
`.env` to change ports or passwords (`.env` is git-ignored).

## Docker setup

```powershell
cd $HOME\Desktop\inventory-order-engine
docker compose up --build          # add -d to run in the background
```

| Service | What | Port |
|---|---|---|
| `postgres` | PostgreSQL 17 (data in the `pgdata` volume) | 5432 |
| `redis` | Redis 7 (no persistence: it's a cache) | 6379 |
| `migrate` | Applies migrations once, then exits | – |
| `api` | HTTP API | 8080 |
| `worker` | Background jobs, `/metrics`, `/health` | 9091 |

Start order is enforced: `postgres` healthy → `migrate` succeeded → `api` and `worker`. The API
and worker have health checks, and every long-running service restarts automatically.

```powershell
docker compose ps                        # all "healthy"?
docker compose logs -f api               # follow logs
docker compose run --rm migrate          # run migrations again (no-op if up to date)
docker compose exec postgres psql -U app -d inventory -c "SELECT * FROM schema_migrations;"
docker compose down                      # stop (data kept)
docker compose down -v                   # stop and DELETE all data
```

The image is a multi-stage build (Go toolchain → ~85 MB Alpine image with three binaries),
runs as a non-root user, and is the same image used in production.

---

## Walkthrough: from zero to a paid order

PowerShell (Windows) with `Invoke-RestMethod`. Bash/curl versions of every call are in
[docs/API.md](docs/API.md).

```powershell
# 1. Start the system (migrations run automatically)
docker compose up --build -d
$base = "http://localhost:8080/api/v1"

# 2. Create a user, then make them an admin (registration always creates CUSTOMERs)
Invoke-RestMethod -Method Post "$base/auth/register" -ContentType "application/json" `
  -Body (@{ email = "alice@example.com"; name = "Alice"; password = "super-secret-1" } | ConvertTo-Json)
docker compose exec postgres psql -U app -d inventory -c "UPDATE users SET role = 'ADMIN' WHERE email = 'alice@example.com';"

# 3. Log in (after promotion, so the token carries the ADMIN role)
$login = Invoke-RestMethod -Method Post "$base/auth/login" -ContentType "application/json" `
  -Body (@{ email = "alice@example.com"; password = "super-secret-1" } | ConvertTo-Json)
$h = @{ Authorization = "Bearer $($login.access_token)" }

# 4. Create a product (price in minor units: 129900 = Rs 1,299.00)
$p = Invoke-RestMethod -Method Post "$base/products" -Headers $h -ContentType "application/json" `
  -Body (@{ sku = "MOUSE-01"; name = "Wireless Mouse"; price = 129900; currency = "INR" } | ConvertTo-Json)

# 5. Add inventory (every product starts with 0)
Invoke-RestMethod -Method Patch "$base/products/$($p.id)/inventory" -Headers $h -ContentType "application/json" -Body '{"adjustment": 10}'

# 6. Place an order: stock is reserved (available 10 -> 8, reserved 0 -> 2)
$order = Invoke-RestMethod -Method Post "$base/orders" -Headers ($h + @{ "Idempotency-Key" = "$([guid]::NewGuid())" }) `
  -ContentType "application/json" -Body (@{ items = @(@{ product_id = $p.id; quantity = 2 }) } | ConvertTo-Json -Depth 3)
Invoke-RestMethod "$base/products/$($p.id)/inventory" -Headers $h

# 7. Pay (mock provider). Try -Body '{"simulate":"FAILURE"}' or '{"simulate":"TIMEOUT"}' on another order
$paid = Invoke-RestMethod -Method Post "$base/orders/$($order.id)/pay" -Headers $h
$paid.order.status          # CONFIRMED
Invoke-RestMethod "$base/products/$($p.id)/inventory" -Headers $h    # available 8, reserved 0 (sold)

# See an error body (Invoke-RestMethod throws on 4xx)
try { Invoke-RestMethod -Method Post "$base/orders/$($order.id)/cancel" -Headers $h } catch { $_.ErrorDetails.Message }
```

**8. Test concurrent orders:** see the next section.

> In Windows PowerShell, `curl` is an alias for `Invoke-WebRequest`; real curl is `curl.exe`.

---

## The three demonstrations

One command runs all three against the Docker stack. It creates a demo admin, raises the rate
limit while the demos run (100 logins from one IP is exactly what the limiter blocks), and
restores it afterwards:

```powershell
.\scripts\demo.ps1
```

Real output:

```text
==> Demo 1: 100 concurrent buyers, stock = 1
Results (100 requests in 298ms)
   201 CREATED                  1
   409 OUT_OF_STOCK            99
Final inventory: available=0 reserved=1
PASS: exactly 1 order(s) succeeded, the rest got OUT_OF_STOCK, stock never went negative.

==> Demo 2: the same Idempotency-Key sent 50 times at once
   201 CREATED (new order)          1
   200 OK (idempotent replay)      49
   distinct order ids returned      1
PASS: one order was created; every retry got that same order back; stock was reserved once.

==> Demo 3: payment failure releases the reservation
4. Order 647 is RESERVED        -> inventory available=3 reserved=2
5. Payment declined         -> 402 PAYMENT_FAILED
6. Order 647 is CANCELLED       -> inventory available=5 reserved=0
PASS: the declined payment released the reservation; all 5 unit(s) are available again.

ALL DEMOS PASSED
```

Run them individually with `go run ./cmd/concurrency-demo -mode stock|idempotency|payment-failure`
(see `-help`).

---

## Testing

```powershell
.\scripts\test.ps1                        # everything: starts Postgres + Redis, gofmt, vet, tests
.\scripts\test.ps1 -Race -Coverage        # + race detector + merged coverage report (coverage.html)
.\scripts\test.ps1 -Run Concurrency -Count 20 -Race   # hammer the concurrency tests
```

Latest run: **396 passed, 0 failed, 0 skipped, race detector on; 89.7 % statement coverage**
(merged across all test binaries).

| Kind | Examples | Where |
|---|---|---|
| Unit | Order totals, state transitions, validation, money, idempotency fingerprints, client IP parsing | `internal/**/…_test.go` |
| Integration | Repositories, transactions, constraints, reservation lifecycle (real PostgreSQL) | `internal/**/…_test.go` |
| Concurrency | 100 buyers / 1 unit; concurrent idempotent requests; concurrent pays; worker vs cancel races | `orders/concurrency_test.go`, `tests/concurrency_api_test.go` |
| Failure | Payment failure and timeout, database down, Redis down, expired reservation, duplicate request, worker retry, outbox dead letter, client disconnect | `orders/failure_test.go`, `tests/failure_api_test.go`, … |
| End-to-end API | Real HTTP through the real router, middleware and services | `tests/` |

Each integration test gets its **own migrated PostgreSQL schema and Redis key prefix**, so
tests never share data and can run in parallel. The full catalogue, mapped to each requirement
and listing real bugs the tests caught, is in
[docs/learning/13-testing.md](docs/learning/13-testing.md).

> Low on memory? A machine with 8 GB running Docker can fail to *link* race-enabled test
> binaries ("paging file is too small"). Run `$env:GOFLAGS="-p=2"` first.

---

## API documentation

**[docs/API.md](docs/API.md)** documents every endpoint: method, URL, authentication, request,
response, possible errors, and a curl example captured from the running API.

| Method | URL | Auth | Purpose |
|---|---|---|---|
| GET | `/health`, `/ready`, `/metrics` | none | Liveness, readiness, Prometheus |
| POST | `/api/v1/auth/register`, `/api/v1/auth/login` | none | Create account, get a JWT |
| GET | `/api/v1/users/me` | Bearer | Current user |
| GET | `/api/v1/products`, `/api/v1/products/{id}` | none | Browse the catalogue |
| POST, PATCH, DELETE | `/api/v1/products[/{id}]` | Admin | Manage products (DELETE = archive) |
| GET, PATCH | `/api/v1/products/{id}/inventory` | Admin | Read, adjust or set stock |
| POST | `/api/v1/orders` | Bearer | Create an order and reserve stock (`Idempotency-Key`) |
| GET | `/api/v1/orders`, `/api/v1/orders/{id}` | Bearer | Your orders (admins: all) |
| POST | `/api/v1/orders/{id}/pay` | Bearer | Pay (mock provider) |
| POST | `/api/v1/orders/{id}/cancel` | Bearer | Cancel and release stock |

Errors always look like
`{"error":{"code":"OUT_OF_STOCK","message":"…","request_id":"…"}}`. Codes are listed in the
[API reference](docs/API.md#error-codes).

## Deployment

**[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** is the step-by-step guide. In short:

- **Backend on Render**, free plan, from [`render.yaml`](render.yaml) (one Blueprint creates
  PostgreSQL, Redis and the API). Secrets are generated by Render; nothing secret is in git.
- **Frontend on Vercel**: root directory `frontend`, `VITE_API_BASE_URL` = the Render URL.
  Then add the Vercel URL to the API's `CORS_ALLOWED_ORIGINS`.

The same Docker image runs locally and in production. Four environment switches adapt it to
a platform-as-a-service host:

| Setting | Why |
|---|---|
| `PORT` | The platform picks the port; the API listens on it |
| `TRUSTED_PROXY_HOPS=1` | Behind a load balancer, read the real client IP from the right end of `X-Forwarded-For` (spoof-proof) |
| `MIGRATE_ON_START=true` | No pre-deploy step on the free plan; the advisory lock keeps it safe |
| `RUN_WORKER=true` | No background workers on the free plan; jobs are concurrency-safe |
| `METRICS_TOKEN` | `/metrics` is public on the internet, so it needs a bearer token |

Why each choice was made: [docs/learning/15-deployment.md](docs/learning/15-deployment.md).

---

## Future improvements

Deliberately left out to keep the project focused. Each one is a natural next step:

| Area | Improvement |
|---|---|
| Payments | A real provider (Stripe/Razorpay) behind the existing `Provider` interface, with signed webhooks and refunds |
| Events | Publish the outbox to a real broker (Kafka, RabbitMQ, SNS); add consumers (emails, analytics) |
| Auth | Refresh tokens and logout (token revocation), email verification, password reset, an admin API for roles |
| Orders | Fulfilment states in use (`PROCESSING → SHIPPED → DELIVERED`), refunds for confirmed orders |
| Inventory | Multiple warehouses, stock movement history (audit table) |
| API | OpenAPI spec generated from code, API versioning policy, cursor pagination for large lists |
| Operations | CI pipeline (GitHub Actions running `scripts/test.ps1` equivalents), Grafana dashboards and alerts, distributed tracing (OpenTelemetry) |
| Scale | Read replicas for product browsing, sliding-window rate limits, partitioning `outbox_events` |
| Security | Rate limits per endpoint (stricter on login), account lockout, audit log of admin actions |

---

## Roadmap (all stages complete)

1. ✅ Project setup, Docker, PostgreSQL
2. ✅ Authentication (register, login, JWT, roles)
3. ✅ Products
4. ✅ Inventory
5. ✅ Orders
6. ✅ Inventory reservations
7. ✅ Transactions and concurrency (100 concurrent buyers, 1 item in stock)
8. ✅ Idempotency keys
9. ✅ Mock payments
10. ✅ Background workers (reservation expiry, payment reconciliation)
11. ✅ Redis (cache and rate limiting)
12. ✅ Transactional outbox
13. ✅ Test suite hardening
14. ✅ Observability (Prometheus metrics, health checks)
15. ✅ Deployment and documentation

Plus a React frontend ([frontend/](frontend/README.md)).
