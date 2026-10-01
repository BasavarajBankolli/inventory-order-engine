# 15 — Deployment: From Laptop to the Internet

## What problem does this solve?

On your laptop, `docker compose up` gives you everything: a database, Redis, the API and the
worker, all on one private Docker network with known ports and throwaway passwords. On the
internet, almost every one of those assumptions breaks:

| On the laptop | In production |
|---|---|
| You choose the port (8080) | The platform chooses it (`PORT=10000` on Render) |
| Clients connect directly | Clients connect to a **load balancer**, which connects to you |
| `migrate` container runs before the API | Maybe no separate step (Render free plan) |
| A separate `worker` container | Maybe no background workers (free plan) |
| `/metrics` only on localhost | Every URL is public |
| Passwords in `docker-compose.yml` defaults | Secrets must be generated and injected |
| Frontend and API on `localhost` | Two different domains → **CORS** |

Stage 15 makes the **same Docker image** run correctly in both worlds by changing only
environment variables.

## Why do we need it?

A project that only runs on your laptop is hard to show anyone. More importantly, deployment
surfaces real engineering problems that interviews ask about: proxies and client IPs, migrations
during deploys, secrets, health checks, cold starts and cost trade-offs.

## How does our implementation work?

### 1. One image, configured by environment (12-factor app)

The Dockerfile builds `api`, `worker` and `migrate` into one image. What differs between
environments is **only** environment variables:

```text
same image ──► laptop:  HTTP_ADDR=:8080, compose passwords, TRUSTED_PROXY_HOPS=0
           └─► Render:  PORT=10000 (from Render), DATABASE_URL from Render, generated JWT_SECRET,
                        TRUSTED_PROXY_HOPS=1, MIGRATE_ON_START=true, RUN_WORKER=true
```

`config.Load` reads `HTTP_ADDR`, falls back to `PORT`, then to `:8080`. Every setting is
validated at startup, so a typo (`RUN_WORKER=maybe`) stops the deploy with a clear error instead
of misbehaving later. That's called **fail fast**.

### 2. Infrastructure as code: `render.yaml`

Instead of clicking through a dashboard (and forgetting what you clicked), `render.yaml`
declares the database, Redis and the API, and wires them together:

```yaml
- key: DATABASE_URL
  fromDatabase: { name: inventory-db, property: connectionString }
- key: JWT_SECRET
  generateValue: true          # Render creates a random secret; it never touches git
```

The file is reviewed in pull requests like code, and recreating the environment is one click.

### 3. Migrations on deploy: `MIGRATE_ON_START`

The ideal order is *migrate, then start the new version*. Paid Render plans have a
**pre-deploy command** for exactly that (`/app/migrate`). The free plan doesn't, so the API can
migrate itself on startup:

```go
if cfg.MigrateOnStart {
    applied, err := database.Migrate(ctx, pool, migrations.FS)   // before serving any request
```

**Is it safe if two instances start at the same time?** Yes. `database.Migrate` takes a
PostgreSQL **advisory lock** first (`pg_advisory_lock`), so the second instance waits, then
sees every migration already applied and does nothing. Each migration also runs in its own
transaction (PostgreSQL supports transactional DDL), so a failed migration leaves no
half-applied schema.

**Rule that makes this work:** migrations must be **backward compatible** with the version
that is still running during a deploy. Add a nullable column, deploy code that uses it, and
only later drop the old column. Never rename a column in one step.

### 4. The worker inside the API: `RUN_WORKER`

Free plans have no background workers, so `RUN_WORKER=true` starts the same jobs in a goroutine
of the API process:

```go
go func() {
    defer close(workerDone)
    worker.Run(ctx, logger.With("component", "worker"), cfg.WorkerInterval, jobs...)
}()
...
<-workerDone   // on shutdown: let the current job finish before closing the DB pool
```

This is safe because the jobs were built for concurrency from Stage 10 on: each order is
handled in its own transaction with row locks, the outbox uses `FOR UPDATE SKIP LOCKED`, and
every job is idempotent. Two API instances, or an API plus a separate worker, can run the jobs
at the same time without double-processing anything.

**Trade-off:** on a paid plan a separate worker is better. A slow batch never competes with
HTTP requests for CPU, and you can scale the two independently.

### 5. Client IPs behind a load balancer: `TRUSTED_PROXY_HOPS`

This one is subtle. On Render, every request reaches the API **from the load balancer**, so
`r.RemoteAddr` is the same for every visitor. A per-IP rate limiter would put *all* anonymous
users in **one** bucket: 100 requests per minute for the whole internet.

The load balancer adds the real client to the `X-Forwarded-For` header. But the client can
**also** send that header, with any IP it likes:

```text
client sends:            X-Forwarded-For: 6.6.6.6          (forged)
Render appends client:   X-Forwarded-For: 6.6.6.6, 203.0.113.7
                                                   ^ written by our proxy: trustworthy
```

Only entries added by proxies **we** control can be trusted, and proxies **append**. So with N
trusted proxies, the real client is the entry **N places from the right**:

```go
ip := hops[len(hops)-trustedHops]   // ClientIP in internal/ratelimit
```

Reading the **leftmost** entry, a common mistake, lets anyone dodge the limit by sending a
fake header. `TRUSTED_PROXY_HOPS=0` (the local default) ignores the header completely.

### 6. Public `/metrics`: `METRICS_TOKEN`

On Render every route is public. Metrics reveal traffic volumes and internal names, so with
`METRICS_TOKEN` set, `/metrics` requires `Authorization: Bearer <token>`. The comparison uses
`crypto/subtle.ConstantTimeCompare`: a normal `==` returns early at the first wrong byte, and
an attacker can measure that time difference to guess the token byte by byte.

### 7. Frontend on Vercel, CORS on the API

The React app is static files on Vercel's CDN; the API is on Render. The browser enforces
**CORS**: JavaScript from `https://shop.vercel.app` may only read responses from
`https://api.onrender.com` if the API answers with
`Access-Control-Allow-Origin: https://shop.vercel.app`. That's why `CORS_ALLOWED_ORIGINS`
must list the exact Vercel origin. CORS protects users' browsers, not your server, and curl
ignores it completely.

## What happens during a normal request?

A customer on the Vercel site clicks **Place order**:

1. The browser sends a CORS **preflight** (`OPTIONS`). The CORS middleware answers it before
   auth, because a preflight carries no token.
2. The real `POST /api/v1/orders` goes to Render's load balancer over HTTPS. TLS ends there, and
   it forwards plain HTTP to the container on `PORT`, adding `X-Forwarded-For`.
3. Middleware: request id → logger → `RequireAuth` (JWT) → rate limiter keyed by
   `user:<id>` (or by `ClientIP(r, 1)` on public routes).
4. The order transaction runs against Render PostgreSQL over the private network.
5. Within `WORKER_INTERVAL`, the in-process worker publishes the `OrderCreated` outbox event.

## What happens when it fails?

| Failure | Behaviour |
|---|---|
| Bad configuration | The process exits at startup with a clear message. Render keeps the **previous** version serving (the new one never passes `/health`) |
| Migration fails | The API doesn't start (the migration rolled back), and the old version keeps serving |
| Redis down / not provisioned | `/ready` → `degraded`; cache and rate limiting are off; orders still work |
| Instance put to sleep (free plan) | Next request wakes it (~1 min). The worker catches up using `expires_at` and the outbox table, because nothing important lives in memory |
| Deploy / restart (`SIGTERM`) | Graceful shutdown: stop accepting connections, finish in-flight requests (≤ `SHUTDOWN_TIMEOUT`), let the worker finish its current job, close the pool |

## What database concepts are involved?

- **Advisory locks** serialise migrations across instances.
- **Transactional DDL:** a failed migration rolls back completely.
- **Connection limits:** small databases allow few connections, so every process sizes its
  pool (`DB_MAX_CONNS=5`). Total connections = instances × pool size, and it must stay under the
  database limit.
- **Private networking:** the database URL from `fromDatabase` uses Render's internal hostname,
  so the database never needs to be reachable from the internet by the app.
- **`citext`** (migration 000001) is a "trusted" extension, so the database owner can create it
  on a managed PostgreSQL without superuser rights.

## What concurrency issues exist?

- **Two instances migrating at once:** solved by the advisory lock.
- **Two copies of the worker** (API with `RUN_WORKER` plus a separate worker, or two API
  instances): safe by design. Row locks and `SKIP LOCKED` mean each order and each event is
  handled once.
- **Old and new versions running together during a deploy:** the reason migrations must be
  backward compatible.
- **Rate limits across instances:** counters live in Redis, so two API instances share one
  budget per client instead of each allowing 100.

## How can I reproduce/test it?

Everything except the actual Render account can be checked locally.

**Simulate Render with the real image** (empty database, platform-style config):

```powershell
docker compose exec -T postgres psql -U app -d inventory -c "CREATE DATABASE render_sim;"
docker run --rm -d --name render-sim --network inventory-order-engine_default -p 10000:10000 `
  -e PORT=10000 -e "DATABASE_URL=postgres://app:app_dev_password@postgres:5432/render_sim?sslmode=disable" `
  -e "JWT_SECRET=render-simulation-secret-0123456789abcdef" -e "REDIS_URL=redis://redis:6379/2" `
  -e MIGRATE_ON_START=true -e RUN_WORKER=true -e TRUSTED_PROXY_HOPS=1 -e RATE_LIMIT_PER_MINUTE=3 `
  inventory-order-engine-api:latest
docker logs render-sim        # migrations applied, listening on :10000, worker started
curl.exe -s http://localhost:10000/ready
# two "visitors" behind the proxy get separate budgets; the forged 6.6.6.6 is ignored:
1..4 | % { curl.exe -s -o NUL -w "%{http_code} " -H "X-Forwarded-For: 6.6.6.6, 203.0.113.1" http://localhost:10000/api/v1/products }
curl.exe -s -o NUL -w "%{http_code}`n" -H "X-Forwarded-For: 203.0.113.2" http://localhost:10000/api/v1/products
docker stop render-sim        # logs: shutdown signal, worker stopped, http server stopped cleanly
docker compose exec -T postgres psql -U app -d inventory -c "DROP DATABASE render_sim;"
```

Expected: `200 200 200 429` for visitor 1, then `200` for visitor 2.

**Tests:**

- `internal/config/config_test.go`: `PORT` fallback, `HTTP_ADDR` precedence, the new switches and their validation.
- `internal/ratelimit/ratelimit_test.go`: `TestClientIP_BehindProxies` (forged entries, multiple hops, missing header, garbage, IPv6) and `TestMiddleware_PerClientBehindProxy`.
- `tests/metrics_api_test.go`: `TestMetricsEndpoint_TokenProtected`.

## Interview questions

1. *Why read the right-most `X-Forwarded-For` entry, not the left-most?* Proxies append. Only entries written by proxies you control are trustworthy, and the left side is client-controlled.
2. *How do you run migrations safely with several instances?* Use a single release step, or an advisory lock, plus backward-compatible migrations (expand, then contract).
3. *Liveness vs readiness on a PaaS?* A failing health check during deploy keeps the old version serving. Liveness must not depend on the database, or a DB blip restarts everything.
4. *What's a 12-factor app?* Config in the environment, logs to stdout, stateless processes, and the same build for every environment.
5. *What does CORS protect against?* Other websites' JavaScript reading your API responses in a user's browser. It isn't server-side security: curl ignores it.
6. *Why is it OK that the free instance sleeps?* All state (orders, reservations, the outbox) is in PostgreSQL with timestamps, so the worker resumes correctly. And payment re-checks `expires_at`, so nothing expired can be bought in the meantime.
