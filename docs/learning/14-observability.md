# 14 — Observability: Logs, Metrics and Health Checks

## What problem does this solve?

Once the service runs somewhere you can't attach a debugger, you have to answer questions like
these without one:

- Is it up? Can it reach PostgreSQL and Redis?
- How many requests per second? How many fail? How slow is the slowest 5 %?
- Are orders failing? Why: out of stock, or bugs?
- Are payments timing out more than usual?
- Is the worker keeping up, or are outbox events piling up and dying?

**Observability** is everything the service tells you about itself. It has three parts:

| Signal | Answers | Here |
|---|---|---|
| **Logs** | "What happened in THIS request?" | one JSON line per request (`log/slog`) |
| **Metrics** | "How is the system doing over time, across ALL requests?" | Prometheus counters and histograms at `GET /metrics` |
| **Health checks** | "Should traffic / restarts happen?" | `GET /health`, `GET /ready`, worker `:9091/health` |

(The fourth classic signal, **traces**, follows one request across many services. With a single
monolith, the request id in every log line covers most of that need.)

## Why do we need it?

Logs alone don't scale for "how often". To know your error rate you'd have to search and count
millions of lines. A metric is a number the service keeps updated, for example
`http_requests_total{status="500"} 42`. Prometheus reads it every 15 s and stores the history.
Then `rate(...)` gives errors per second and an alert can fire when it crosses a threshold.

Metrics alone can't explain *why*. You see "5xx went up". Then you go to the logs, filter by
`route` and `status`, and read the individual failures. Each signal leads you to the next.

## How does our implementation work?

### Metrics (`internal/metrics`)

```text
      API process                                      Worker process
 ┌─────────────────────────┐                     ┌──────────────────────────┐
 │ Logger middleware ──────┼─ http_requests_total │ expire job ──────────────┼─ inventory_reservations_total{event=expired}
 │ orders.Service ─────────┼─ orders_*, payments_*│ publish-outbox job ──────┼─ outbox_events_processed_total
 │ products.Service ───────┼─ product_cache_*     │                          │
 │ ratelimit.Middleware ───┼─ http_rate_limited_* │ GET :9091/metrics        │
 │ GET :8080/metrics       │                     │ GET :9091/health         │
 └────────────▲────────────┘                     └────────────▲─────────────┘
              └──────────── Prometheus scrapes both ─────────┘
```

**One `*metrics.Metrics` per process**, created in `main` and passed down (dependency
injection). It has its **own registry**, not Prometheus's global default, so every test can
create a fresh one and check exact numbers. Every method is **nil-safe**: a service built with
`nil` metrics (most tests) just records nothing.

| Metric | Type | Labels | Recorded where |
|---|---|---|---|
| `http_requests_total` | counter | method, route, status | Logger middleware |
| `http_request_duration_seconds` | histogram | method, route | Logger middleware |
| `orders_created_total` | counter | – | `orders.CreateWithKey`, only after commit, not on replay |
| `orders_failed_total` | counter | reason | `orders.CreateWithKey` |
| `inventory_reservations_total` | counter | event = created / confirmed / released / expired | orders: create, pay, cancel, expiry |
| `payments_success_total` | counter | – | `completePayment` |
| `payments_failed_total` | counter | reason = declined / timeout | `Pay`, `completePayment` |
| `outbox_events_processed_total` | counter | result = published / retry / dead | worker `publish-outbox` |
| `product_cache_requests_total` | counter | result = hit / miss / error | `products.Get` |
| `http_rate_limited_total` | counter | – | rate-limit middleware (on 429) |
| `go_*`, `process_*` | gauges | – | built-in collectors: goroutines, memory, GC, CPU, open fds |

**Counter vs histogram.** A *counter* only goes up, and Prometheus computes rates from it. A
*histogram* sorts each request duration into buckets (≤5 ms, ≤10 ms, … ≤10 s). From the buckets
Prometheus can estimate percentiles:

```promql
# requests per second, per route
sum by (route) (rate(http_requests_total[5m]))

# share of 5xx responses
sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))

# p95 latency of order creation
histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{route="/api/v1/orders",method="POST"}[5m])))

# cache hit ratio
sum(rate(product_cache_requests_total{result="hit"}[5m])) / sum(rate(product_cache_requests_total[5m]))
```

**Why averages lie:** if 95 requests take 10 ms and 5 take 4 s, the average is about 210 ms,
which looks fine. The p95 is 4 s, so 1 user in 20 is waiting 4 seconds. Always alert on
percentiles.

### Labels and cardinality (the most important rule)

Every distinct combination of label values is a separate **time series** stored by Prometheus.
`route="/api/v1/orders/{id}"` is ONE series. Labelling by raw path
(`/api/v1/orders/1`, `/api/v1/orders/2`, …) would create a new series **per order**. That
"cardinality explosion" eats memory until the monitoring system falls over. So:

- The route label is chi's **route pattern** (`chi.RouteContext(r).RoutePattern()`), read after the
  handler ran. Requests that matched no route are all labelled `unmatched`, so a bot probing
  10 000 random URLs adds one series, not 10 000.
- Reasons come from **fixed small sets** (`failureReason` maps errors to 6 values).
- Never put user ids, emails, SKUs or error messages in labels. Those belong in **logs**.

### Count what HAPPENED, exactly once

Business metrics are recorded **after the transaction commits**, never inside it:

- An order that hits out-of-stock on its 2nd line rolls back. It counts as
  `orders_failed_total{reason="out_of_stock"}` and **not** as created, even though line 1 was
  briefly reserved inside the transaction.
- An idempotent **replay** returns the stored order. It doesn't count as a new order.
- Paying twice (double click, two tabs, ten concurrent calls) counts **one** success. The counter
  only moves when *this* call actually moved the reservations (`finished > 0`). The calls that
  found the order already paid record nothing.
- Cancelling an already-cancelled order, or running expiry twice, records nothing the second
  time.

Each of these cases has a test in `internal/orders/metrics_test.go`.

### Logs (`internal/middleware/logger.go`)

One line per request, now with `route` and `user_id`:

```json
{"level":"INFO","msg":"http request","method":"POST","path":"/api/v1/orders/539/pay",
 "route":"/api/v1/orders/{id}/pay","status":200,"bytes":481,"duration_ms":74.21,
 "user_id":557,"request_id":"8a1accf6..."}
```

**How does an OUTER middleware know the user?** The Logger runs *before* `RequireAuth`, and
RequireAuth adds the user to a *new* request context that the Logger never sees. The fix is
`identity.WithSlot`. The Logger puts an empty, mutable slot in the context. `identity.NewContext`
(called by RequireAuth) also fills that slot. After the handler returns, the Logger reads it with
`identity.FromSlot`.

Never logged: query strings, headers, bodies, tokens and passwords. They can contain secrets
or personal data.

### Health checks

| Endpoint | Meaning | Used by |
|---|---|---|
| `GET /health` | The process is alive (no dependency checks) | Docker healthcheck; Kubernetes *liveness*: if it fails, restart |
| `GET /ready` | PostgreSQL is reachable (503 if not); Redis status reported as `degraded` | Load balancer / *readiness*: if it fails, stop sending traffic, don't restart |
| worker `GET :9091/health` | The worker process is alive | Docker healthcheck of the `worker` service |

Why are they different? If PostgreSQL is down, restarting the API won't fix it. That would only
turn one outage into a restart loop. So liveness stays green, readiness turns red, and traffic
goes elsewhere until the database is back.

### The worker's tiny HTTP server

The worker has no API, but Prometheus still needs its metrics: expirations happen *in the worker*,
not in the API. `cmd/worker/main.go` starts a separate `http.ServeMux` on
`WORKER_METRICS_ADDR` (default `:9091`) with only `/metrics` and `/health`. It shuts down
gracefully with the worker.

## What happens during a normal request?

`POST /api/v1/orders/42/pay` by user 7:

1. `RequestID` sets the id. The Logger notes the start time and adds the identity slot.
2. `RequireAuth` validates the JWT and fills the slot with user 7.
3. The per-user rate limiter allows the request (otherwise 429 and `http_rate_limited_total++`).
4. `orders.Pay` charges the provider and commits. Then `payments_success_total++` and
   `inventory_reservations_total{event="confirmed"} += lines`.
5. The handler writes 200. The Logger calls
   `ObserveHTTP("POST", "/api/v1/orders/{id}/pay", 200, 0.074)` and writes the log line with
   `user_id=7`.
6. Within 15 s Prometheus scrapes `/metrics` and stores the new values.

## What happens when it fails?

| Failure | What you see |
|---|---|
| Panic in a handler | Recoverer returns 500. The metric shows `status="500"`, and the log has the stack trace |
| PostgreSQL down | `/ready` → 503, `orders_failed_total{reason="internal"}` and 5xx rates climb; `/health` stays 200 |
| Redis down | `product_cache_requests_total{result="error"}` rises, and requests still succeed (fallback) |
| Payment provider slow | `payments_failed_total{reason="timeout"}` rises, and the worker later reconciles |
| Outbox broker failing | `outbox_events_processed_total{result="retry"}`, then `dead`, rise |
| Metrics code itself | Can't break a request: methods are no-ops on nil and never return errors |

`/metrics` is **unauthenticated** here (convenient locally). In production, keep it on an
internal network or behind the load balancer. It reveals traffic volumes, not data.

## What database concepts are involved?

Mostly none, on purpose. Metrics live in process memory and cost nanoseconds; they never touch
PostgreSQL. A restart resets counters to 0, and Prometheus's `rate()` detects and handles that
("counter reset"). What matters is *where* the recording happens: **after COMMIT**. Code that
records inside the transaction would count work that later rolls back.

## What concurrency issues exist?

- Prometheus counters are atomic. Many goroutines can `Inc()` at once safely, and the race
  detector run (`scripts/test.ps1 -Race`) confirms it.
- The real issue is **double counting under races**: two concurrent `Pay` calls on one order.
  Only the call whose transaction actually moved the reservations from ACTIVE (row-locked, so
  exactly one wins) records the success. `TestMetrics_ConcurrentPaysCountOnce` fires 10 at once
  and expects exactly 1.
- With several API replicas, each has its own counters. Prometheus scrapes each replica, and
  you `sum()` across them in queries.

## How can I reproduce/test it?

```powershell
docker compose up -d --build
# make some traffic (login, browse, order, pay), then:
curl.exe -s http://localhost:8080/metrics | Select-String "^(http_requests_total|orders_|payments_|product_cache)"
curl.exe -s http://localhost:9091/metrics | Select-String "^outbox_"
curl.exe -s http://localhost:9091/health
docker compose logs api --tail 5        # JSON lines with route and user_id
docker compose ps                       # api and worker both show (healthy)
```

Tests:

```powershell
.\scripts\test.ps1 -Run "Metric|Logger|WorkerJobs|Cache_Metrics|Returns429"
```

- `internal/metrics/metrics_test.go`: every method, nil safety, the text format, independent registries.
- `internal/orders/metrics_test.go`: created/replay, failure reasons and rollback, payment once (sequential and concurrent), declined, timeout then retry, cancel/expire idempotent.
- `internal/middleware/middleware_test.go`: route pattern vs raw path, `unmatched`, `user_id` in log.
- `internal/products/cache_test.go`: hit / miss / error.
- `internal/ratelimit/ratelimit_test.go`: only rejected requests count.
- `internal/app/worker_test.go`: the worker counts published outbox events.
- `tests/metrics_api_test.go`: real HTTP traffic, then `GET /metrics`; checks that no raw id leaked into labels.

**Try it with Prometheus** (optional, not part of the compose file):

```yaml
# prometheus.yml
scrape_configs:
  - job_name: inventory-api
    static_configs: [{ targets: ["host.docker.internal:8080"] }]
  - job_name: inventory-worker
    static_configs: [{ targets: ["host.docker.internal:9091"] }]
```

```powershell
docker run --rm -p 9090:9090 -v ${PWD}/prometheus.yml:/etc/prometheus/prometheus.yml prom/prometheus
# open http://localhost:9090 and try the queries above
```

## Interview questions

1. *Logs vs metrics vs traces?* Logs explain one event, metrics show aggregates over time, and traces follow one request across services.
2. *Why not label metrics with the user id or the URL path?* Cardinality: each distinct value creates a new time series.
3. *Liveness vs readiness?* Failed liveness means restart. Failed readiness means stop routing traffic. A down database should fail readiness only.
4. *Why p95/p99 instead of the average?* Averages hide the slow tail that real users feel.
5. *How do you avoid counting a rolled-back order?* Record after commit, and only when this call actually changed the state.
6. *Counter vs gauge vs histogram?* A counter only goes up (requests). A gauge goes up and down (goroutines, queue length). A histogram holds a distribution in buckets (latency).
