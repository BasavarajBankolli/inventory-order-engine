# 01 — Architecture & Project Skeleton

## What problem does this solve?

Before writing any business feature, a backend needs a solid base:

- a way to **configure** the app (database address, ports) without changing code;
- a **database connection** that is shared safely by many concurrent requests;
- a repeatable way to **create and evolve the database schema** (migrations);
- **consistent HTTP behaviour**: JSON errors, request IDs, logs, crash protection;
- **health endpoints** so Docker and load balancers know whether the app works;
- a **one-command local environment** (`docker compose up`).

Stage 1 builds exactly this. It adds no business features yet.

## Why a modular monolith?

| | Modular monolith (our choice) | Microservices |
|---|---|---|
| Deployment | 1 binary | Many services |
| Calls between modules | Function calls (fast, no failure modes) | Network calls (slow, can fail) |
| Transactions | One PostgreSQL transaction covers order + inventory | Needs sagas and distributed coordination |
| Debugging | One process, one debugger | Traces across services |

"Never oversell" is much easier when **one database transaction** can lock the inventory row
and create the order together. We still keep modules separate (`internal/orders`,
`internal/inventory`, and so on) so one of them could be extracted later if there were a real need.

## How does our implementation work?

### Startup (`cmd/api/main.go`)

```text
config.Load()             read env vars; fail fast if DATABASE_URL is missing
logging.New()             JSON logger; slog.SetDefault once
signal.NotifyContext()    ctx is cancelled on Ctrl+C / docker stop
database.Connect()        create pgx pool + Ping (fail fast on a bad password)
server.NewRouter()        routes + middleware
srv.ListenAndServe()      in a goroutine
<wait for signal>
srv.Shutdown()            finish in-flight requests, then exit
```

`main()` only calls `run()`. `run()` returns an error instead of calling `os.Exit`, so
every `defer` (such as `pool.Close()`) still runs.

### Middleware chain (`internal/middleware`)

```text
request ─► RequestID ─► Logger ─► Recoverer ─► handler
```

1. **RequestID** reuses a safe incoming `X-Request-ID` or generates one, stores it in
   `context.Context`, and echoes it in the response header.
2. **Logger** times the request and writes one JSON log line after the handler finishes.
   It wraps `http.ResponseWriter` so it can read the status code afterwards.
3. **Recoverer** turns a panic into a JSON 500. The stack trace goes to the logs, never to the client.

The order matters. Recoverer runs *inside* Logger, so a panic is still logged as `status: 500`.

### Why is `requestid` its own package?

`middleware`, `logging`, and `httpx` all need the request ID. If it lived in `middleware`, then
`httpx` would import `middleware`, and `middleware` (which writes JSON errors) would import
`httpx`. That is an **import cycle**, and Go refuses to compile it. A tiny shared package
breaks the cycle.

### Automatic `request_id` in every log line (`internal/logging`)

`contextHandler` wraps slog's JSON handler. Whenever code logs with a context, as in
`slog.InfoContext(ctx, "order created", "order_id", id)`, the handler takes the request ID
out of `ctx` and adds it to the line. Business code never has to pass the ID around by hand.

### Migrations (`internal/database/migrate.go`)

```text
acquire 1 connection from pool
SELECT pg_advisory_lock(727274001)       only one migrator at a time
CREATE TABLE IF NOT EXISTS schema_migrations
for each *.sql file in name order:
    already in schema_migrations? skip
    BEGIN
      run the file
      INSERT INTO schema_migrations(version)
    COMMIT                                all or nothing
SELECT pg_advisory_unlock(...)
```

The SQL files are compiled into the binary with `//go:embed` (see `migrations/migrations.go`).

### Readiness vs. liveness (`internal/health`)

- `/health` never touches the database. If the process can answer, it is alive.
- `/ready` pings every dependency, each with a 2-second deadline. If any check fails, it returns **503**.

If PostgreSQL is down, restarting the API won't help. So the API stays *alive* but reports
*not ready*, and traffic is routed elsewhere until the database is back.

## What happens during a normal request?

`curl.exe http://localhost:8080/ready`

1. `net/http` accepts the connection and calls the chi router.
2. `RequestID` generates `4f2a…` and puts it in the context and the response header.
3. `Logger` records the start time and wraps the ResponseWriter.
4. `Recoverer` sets up its `defer recover()`.
5. chi matches `GET /ready` to `health.Handler.Ready`.
6. `Ready` calls `pool.Ping(ctx)` with a 2-second timeout. It succeeds.
7. It writes `200 {"status":"ready","checks":{"postgres":"ok"}}`.
8. `Logger` writes `{"msg":"http request","status":200,"duration_ms":1.3,"request_id":"4f2a…"}`.

## What happens when it fails?

| Failure | Behaviour |
|---|---|
| `DATABASE_URL` missing | `config.Load` returns an error and the process exits with code 1 and a clear message |
| Wrong DB password / DB down at startup | `Connect` pings and fails fast (exit 1) instead of failing on the first user request |
| DB goes down while running | `/health` returns 200, `/ready` returns 503 `postgres: unavailable`; the real error is logged at WARN |
| A handler panics | Recoverer logs the stack and returns JSON 500 with the request_id; the server keeps running |
| Unknown URL / wrong method | JSON 404 / 405 in the standard error format |
| A migration has a SQL error | Its transaction rolls back and the file is not recorded; `migrate` exits 1, so compose never starts `api` |
| Two migrators start together | The second waits on the advisory lock, then sees the file is already applied and skips it |
| `docker stop` / Ctrl+C | `Shutdown` stops accepting connections and waits up to 10 seconds for in-flight requests |

## What database concepts are involved?

- **Connection pool**: reuses a few open connections. `*pgxpool.Pool` is safe to share between goroutines.
- **Transactional DDL**: PostgreSQL can roll back `CREATE TABLE`. MySQL cannot. This is why a failed migration leaves no half-built tables.
- **Advisory lock**: an application-defined lock identified by a number. PostgreSQL enforces it but does not attach it to any table.
- **Extensions**: `citext` gives case-insensitive text, which Stage 2 uses for emails.
- **`search_path`**: which schema unqualified table names resolve to. The tests use it to give each test a private, empty schema.

## What concurrency issues exist?

- **Two migrators at once** (for example, two API replicas that run migrations): both would try to apply `000002`, and one would crash on "table already exists". The advisory lock serialises them. `TestMigrate_ConcurrentRunnersApplyEachMigrationOnce` starts 5 at the same time and checks the migration ran exactly once.
- **Shared state between requests**: there is none. Every request-specific value (such as the request ID) travels in `context.Context`. The pool is designed for concurrent use.
- **Advisory locks are per session**: that's why `Migrate` holds one connection for the whole run. If each query picked a random pooled connection, the lock and unlock could run on different sessions.

## How can I reproduce/test it?

```powershell
docker compose up --build -d
curl.exe -i http://localhost:8080/health
curl.exe http://localhost:8080/ready

# Your own request id is echoed back and appears in the logs
curl.exe -H "X-Request-ID: my-debug-id-1" http://localhost:8080/nope
docker compose logs api | Select-String my-debug-id-1

# Break the database and watch readiness change
docker compose stop postgres
curl.exe http://localhost:8080/ready     # 503
docker compose start postgres
curl.exe http://localhost:8080/ready     # 200

# Migrations are idempotent
docker compose run --rm migrate          # "newly_applied":0

# Tests
go test ./...
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 -v ./internal/database/
```

### Experiments to try

1. Add `panic("test")` to `Health` in `internal/health/handler.go`, restart, and call `/health`. You get a JSON 500 and a stack trace in the logs.
2. Add `migrations/000002_broken.sql` containing `SELEC 1;` and run `go run ./cmd/migrate`. It fails, and `schema_migrations` is unchanged. Delete the file afterwards.
3. Swap the order of `r.Use(middleware.Logger(...))` and `r.Use(middleware.Recoverer(...))` in `internal/server/router.go`, then repeat experiment 1. What does the log line show now, and why?
