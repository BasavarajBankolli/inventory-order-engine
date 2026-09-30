# Inventory & Order Management Engine

A production-style backend in Go that manages products, inventory, and orders.
It is built to handle concurrent orders correctly and **never oversell stock**.

This is a learning and portfolio project, so the code favours clarity over cleverness.
Each module has a matching explanation in [`docs/learning/`](docs/learning/).

> **Status: Stage 1 of 15 — project skeleton, Docker, PostgreSQL, migrations, health checks.**
> This README grows with each stage.

---

## Architecture (so far)

```mermaid
flowchart LR
    client([Client / curl]) -->|HTTP :8080| api
    subgraph docker compose
        migrate[migrate<br/>runs once, exits] -->|applies SQL| pg[(PostgreSQL 17)]
        api[api<br/>Go HTTP server] -->|pgx pool| pg
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
    router --> h[/health/]
    router --> r[/ready/]
    router --> nf[JSON 404 / 405]
```

### Folder layout

```text
cmd/
  api/main.go          Starts the HTTP server (wiring only, no business logic)
  migrate/main.go      Applies pending SQL migrations, then exits
internal/
  config/              Reads settings from environment variables
  database/            PostgreSQL pool + migration runner
  health/              /health (liveness) and /ready (readiness)
  httpx/               Shared JSON response and error helpers
  logging/             Structured JSON logger (log/slog) that adds request_id automatically
  middleware/          RequestID, Logger, Recoverer
  requestid/           Stores and reads the request ID in context.Context
  server/              Router: maps URLs to handlers
  testutil/            Helpers used only by tests
migrations/            Numbered .sql files, embedded into the binary
docker/postgres/init/  One-time Postgres setup (creates the test database)
docs/learning/         Beginner-friendly explanations for every module
```

Packages such as `auth/`, `products/`, `inventory/`, `orders/`, `payments/`, `events/`,
`worker/`, and `cache/` are added in later stages. There is no `pkg/` folder, because
nothing here is meant to be imported by other projects.

## Technology choices

| Need | Choice | Why |
|---|---|---|
| Language | Go 1.26 | Simple, fast, great concurrency and standard library |
| HTTP router | `go-chi/chi/v5` | Tiny, built on `net/http` types, path params like `/products/{id}` |
| PostgreSQL driver | `jackc/pgx/v5` | The most widely used Postgres driver for Go, with a built-in pool |
| Logging | `log/slog` (stdlib) | Structured JSON logs with no dependency |
| Config | `os.Getenv` (stdlib) | Env vars are enough; no config library needed |
| Migrations | Own ~100-line runner | Easy to read end to end; uses advisory locks and transactional DDL |
| Database | PostgreSQL 17 | Source of truth: transactions, row locks, constraints |

Redis is added in Stage 11.

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

# Unit + integration tests (needs the postgres container running)
docker compose up -d postgres
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 ./...

# Verbose output for one package
go test -count=1 -v ./internal/database/
```

Integration tests use a separate `inventory_test` database, created automatically the first
time the Postgres volume is initialised. Each migration test runs inside its own temporary
schema.

---

## API

| Method | URL | Auth | Description |
|---|---|---|---|
| GET | `/health` | none | Liveness: returns 200 if the process is running |
| GET | `/ready` | none | Readiness: returns 200 if dependencies are reachable, otherwise 503 |

```powershell
curl.exe -i http://localhost:8080/health
# 200 {"status":"ok"}

curl.exe http://localhost:8080/ready
# 200 {"status":"ready","checks":{"postgres":"ok"}}
# 503 {"status":"not_ready","checks":{"postgres":"unavailable"}}
```

Every response has an `X-Request-ID` header. Every error uses this shape:

```json
{ "error": { "code": "NOT_FOUND", "message": "the requested resource was not found", "request_id": "..." } }
```

> Use `curl.exe`, not `curl`. In Windows PowerShell, `curl` is an alias for `Invoke-WebRequest`.

---

## Roadmap

1. ✅ Project setup, Docker, PostgreSQL
2. Authentication (register, login, JWT, roles)
3. Products
4. Inventory
5. Orders
6. Inventory reservations
7. Transactions and concurrency (100 concurrent buyers, 1 item in stock)
8. Idempotency keys
9. Mock payments
10. Background workers (reservation expiry)
11. Redis (cache and rate limiting)
12. Transactional outbox
13. Test suite hardening
14. Observability (Prometheus metrics)
15. Deployment and full documentation
