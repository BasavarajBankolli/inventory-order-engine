# Inventory & Order Management Engine

A production-style backend in Go that manages products, inventory, and orders.
It is built to handle concurrent orders correctly and **never oversell stock**.

This is a learning and portfolio project, so the code favours clarity over cleverness.
Each module has a matching explanation in [`docs/learning/`](docs/learning/).

> **Status: Stage 2 of 15 — skeleton + authentication (register, login, JWT, roles).**
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
    router --> h[/health, /ready/]
    router --> pub[/auth/register, /auth/login/]
    router --> ra[RequireAuth] --> me[/users/me/]
```

Handlers stay thin. Business rules live in services, and SQL lives in repositories:

```text
HTTP handler  ->  service (rules)  ->  repository (SQL)  ->  PostgreSQL
auth.Handler      auth.Service         users.Repository
```

### Folder layout

```text
cmd/
  api/main.go          Starts the HTTP server (wiring only, no business logic)
  migrate/main.go      Applies pending SQL migrations, then exits
internal/
  app/                 Builds every module and connects them (used by main and API tests)
  auth/                Register/login service, bcrypt, JWT, RequireAuth/RequireRole middleware
  config/              Reads settings from environment variables
  database/            PostgreSQL pool + migration runner
  health/              /health (liveness) and /ready (readiness)
  httpx/               Shared JSON helpers: strict body decoding, response and error helpers
  identity/            Principal (user id + role) of the authenticated caller, in context
  logging/             Structured JSON logger (log/slog) that adds request_id automatically
  middleware/          RequestID, Logger, Recoverer
  requestid/           Stores and reads the request ID in context.Context
  server/              Router: maps URLs to handlers
  testutil/            Helpers used only by tests (isolated, migrated test schemas)
  users/               User model, users repository (SQL), GET /users/me
  validate/            Collects per-field validation errors
migrations/            Numbered .sql files, embedded into the binary
docker/postgres/init/  One-time Postgres setup (creates the test database)
docs/learning/         Beginner-friendly explanations for every module
tests/                 End-to-end API tests (HTTP -> router -> services -> PostgreSQL)
```

Packages such as `products/`, `inventory/`, `orders/`, `payments/`, `events/`, `worker/`,
and `cache/` are added in later stages. There is no `pkg/` folder, because
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

# Unit + integration tests (needs the postgres container running)
docker compose up -d postgres
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
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
| Repository integration | `internal/users/repository_test.go`, `internal/database/migrate_test.go` | yes |
| End-to-end API | `tests/` | yes |

---

## API

All business endpoints are under `/api/v1`. Request and response details are in
[docs/learning/02-authentication.md](docs/learning/02-authentication.md). A complete API
reference is added in Stage 15.

| Method | URL | Auth | Description | Errors |
|---|---|---|---|---|
| GET | `/health` | none | Liveness: returns 200 if the process is running | – |
| GET | `/ready` | none | Readiness: returns 200 if dependencies are reachable, otherwise 503 | 503 |
| POST | `/api/v1/auth/register` | none | Create a CUSTOMER account | 400 `VALIDATION_ERROR`, 409 `EMAIL_ALREADY_EXISTS` |
| POST | `/api/v1/auth/login` | none | Exchange email and password for a JWT | 400, 401 `INVALID_CREDENTIALS` |
| GET | `/api/v1/users/me` | Bearer | Profile of the logged-in user | 401 `UNAUTHENTICATED` |

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
