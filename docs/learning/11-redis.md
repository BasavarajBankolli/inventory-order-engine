# 11 — Redis: Caching, Rate Limiting & Failing Safely

## What problem does this solve?

1. **Repeated reads.** `GET /products/{id}` is by far the most common request (every product page view). Each one hits PostgreSQL, even though products rarely change.
2. **Abusive clients.** One buggy client, scraper or password-guesser can send thousands of requests per minute and slow the API down for everyone.

Redis is an in-memory key-value store that answers in well under a millisecond. We use it for
both jobs, **but never as the source of truth**.

## Why do we need it?

| | Without Redis | With Redis |
|---|---|---|
| Product page view | 1 PostgreSQL query every time | Served from memory. PostgreSQL only on a miss |
| Measured locally (`GET /products/2`) | miss: 424 ms (first request, cold pool) | **hit: 9 ms** |
| Client sending 10,000 req/min | All reach the database | 100 allowed per minute, the rest get `429` |
| Several API instances | Each needs its own counters (clients get N × the limit) | One shared counter in Redis |

## How does our implementation work?

### The golden rule: Redis is optional and never authoritative

| Data | Where the truth lives | What Redis holds |
|---|---|---|
| Products | PostgreSQL | A **copy** that can be deleted at any time |
| Inventory, orders, payments | PostgreSQL | **Nothing.** Never cached (see below) |
| Rate-limit counters | Redis | Counters that are fine to lose |

The Redis container runs **without persistence** (`--save "" --appendonly no`). If it restarts
empty, nothing is lost.

### 1. Product cache: cache-aside (`products.Service.Get`, `cache.ProductCache`)

```text
GET /products/42
   │
   ├─► Redis GET product:v1:42 ── hit ──► return (no database query)
   │                          └── miss
   ├─► PostgreSQL SELECT ... WHERE id = 42
   └─► Redis SET product:v1:42 <json> EX 300  ──► return
```

- **Invalidation.** `PATCH` and `DELETE` (archive) **delete** the key *after* the database change commits, and the next `GET` loads fresh data. Deleting is simpler and safer than writing the new value into the cache.
- **The TTL** (`PRODUCT_CACHE_TTL`, 5 min) is the safety net. If an invalidation is ever missed, the stale entry disappears on its own.
- **Not-found results aren't cached**, so a newly created product is visible immediately.
- **The key version `v1`** means that if the `Product` shape changes, bumping it to `v2` makes old entries harmless.
- **`products.Cache` is an interface** in the products package. The service doesn't know about Redis, and with `cache == nil` it simply always reads PostgreSQL.

**Why stale product data can never corrupt an order.** Cache-aside has a known race:

```text
reader: cache miss → SELECT (old price 100)
writer:                              UPDATE price = 200 → DEL cache key
reader:                                                             SET cache ← 100 (stale!)
```

The stale entry lives until its TTL. That's acceptable **for display**, because order creation
**never reads the cache**: it reads the product from PostgreSQL with `FOR SHARE` inside the
order transaction (Stage 7). The price a customer pays is always the real one. This is also why
inventory is never cached: "5 in stock" from a cache could be wrong the moment you read it.

### 2. Rate limiting: fixed window (`ratelimit.Limiter`, `ratelimit.Middleware`)

```text
key = rl:<subject>:<window-start>          e.g. rl:user:42:1790771940
MULTI
  INCR   key            → 1, 2, 3 ... (atomic, even with 1000 concurrent requests)
  EXPIRE key 61 NX      → the counter deletes itself after its window
EXEC
count <= limit ? allow : 429
```

- **Who is counted.**
  - Public routes (register, login, product browsing): **per IP** (`ByIP`), because there is no user yet.
  - Authenticated routes: **per user** (`ByUser`), which is fairer. Many users behind one office NAT don't share a budget.
- **`X-Forwarded-For` is ignored.** Any client can put any IP in that header to dodge the limit. Behind a real load balancer you'd trust only *its* header.
- **Headers:** `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and on a 429 also `Retry-After` (seconds until the window resets).
- **`/health` and `/ready` are never limited**, because infrastructure probes them constantly.
- **Fixed-window trade-off:** a client can squeeze in up to 2 × limit requests around a window boundary (end of one minute plus start of the next). Sliding windows or token buckets smooth that out, at the cost of complexity.

`TestAllow_ConcurrentRequestsAreCountedExactly`: 200 simultaneous requests with a limit of 100
give **exactly 100 allowed**. `INCR` is atomic inside Redis, so no increment is lost. That's the
same "let the data store be the referee" idea as the database constraints in earlier stages.

### 3. Failing safely when Redis is down, and the bug we found

| Component | If Redis is down |
|---|---|
| Product cache | Treated as a miss, so the product is read from PostgreSQL. Slower, still correct |
| Rate limiter | **Fails open**: the request is allowed and a warning logged. A counter-store outage mustn't become an API outage (a bank's login might choose to fail *closed*) |
| `/ready` | `200 {"status":"degraded","checks":{"redis":"unavailable"}}`. PostgreSQL is **required** (503 if down), Redis is **optional** |

**What actually happened when we tested it live.** The first version passed its unit tests,
which simulated "Redis down" with a closed local port (fails instantly). Then we stopped the
Redis container in Docker, and **product requests took 65–98 seconds** before the connection
was dropped. Two things combined:

1. In Docker, looking up the hostname `redis` of a *stopped* container **hangs** instead of failing (`lookup redis: i/o timeout`).
2. The go-redis client **retries a failed dial 5 times by default** (`DialerRetries`), and **ignores context deadlines by default** (`ContextTimeoutEnabled = false`).

Each Redis call took about 11 s, and a request makes up to three (rate limit, cache read, cache
write). The fix is in `internal/cache/redis.go` and `breaker.go`:

- **Tight client settings:** 500 ms dial, 300 ms read/write, `DialerRetries = 1`, `ContextTimeoutEnabled = true`.
- **A circuit breaker** (a go-redis hook, so it covers the cache *and* the limiter):

```text
CLOSED ── connection error ──► OPEN (5 s): every Redis call fails INSTANTLY with ErrRedisBypassed
   ▲                              │
   └── probe succeeds ◄── HALF-OPEN: the next call after 5 s is let through as a probe
```

Measured after the fix, with the same test (Redis container stopped):

| | Before | After |
|---|---|---|
| Product request | 65,000–98,000 ms, then failure | 95 ms, then **4–6 ms** each |
| Cost of the outage | Every request | One ~1 s probe every 5 s |
| Recovery | – | Automatic when Redis returns |

`TestRedisClient_FailsFastWhenRedisHangs` and `TestRedisAPI_WorksWhenRedisIsDown` now reproduce
the *hanging* case, using a non-routable address, so this regression can't come back unnoticed.

**The lesson:** "fails fast" and "fails slowly" are different failure modes, and slow failures
are the dangerous ones. Always test with the failure you'll really get.

## What happens during a normal request?

`GET /api/v1/products/2` (anonymous, Redis up):

1. `RequestID` → `Logger` → `Recoverer` run first.
2. The rate limiter runs `MULTI INCR rl:ip:172.19.0.1:… EXPIRE … EXEC` and gets count 3, which is ≤ 100, so the request continues. It sets `X-RateLimit-Remaining: 97`.
3. `products.Service.Get` does `GET product:v1:2` and gets a **hit**, so it returns the JSON (9 ms in total).

`PATCH /api/v1/products/2 {"price": 199900}` (admin): PostgreSQL `UPDATE` commits, then
`DEL product:v1:2`, and the next `GET` is a miss that reloads the new price.

## What happens when it fails?

| Failure | Result |
|---|---|
| Over the limit | `429 RATE_LIMITED` with `Retry-After` |
| Redis down or hanging | The API keeps working (see table above). `/ready` says `degraded`. Logs say `redis unreachable; bypassing it` about once per 5 s |
| Corrupt cache entry | Decode error → logged → treated as a miss → DB |
| Invalidation fails | Logged. The stale entry expires within `PRODUCT_CACHE_TTL` |
| `REDIS_URL` not set | The API logs a warning and runs with no cache and no rate limiting |
| Redis restarts empty | Cache misses warm it up again. Rate-limit counters restart at 0 |

## What database concepts are involved?

- **Cache-aside / lazy loading**, **TTL-based expiry**, and **explicit invalidation** (delete on write).
- **Atomic counters** (`INCR`) and **MULTI/EXEC** transactions (INCR and EXPIRE together, so a counter never lives forever).
- **`EXPIRE ... NX`**: set a TTL only if the key has none (Redis 7+).
- **Key design:** namespacing (`product:v1:`, `rl:`), versioning, and per-test prefixes (`testutil.NewRedis`) so parallel tests don't collide.
- **In-memory, no persistence:** a choice you can only make because PostgreSQL holds the truth.

## What concurrency issues exist?

| Issue | Handling |
|---|---|
| Many requests incrementing one counter at once | `INCR` is atomic, and 200 → exactly 100 allowed is tested |
| Stale-set race in cache-aside (see above) | Bounded by the TTL. Orders never read the cache |
| Several API instances | They share Redis, so they share counters and cache |
| Thundering herd (a popular key expires and 1,000 requests all miss at once) | Not handled. Each miss is one cheap PK lookup at this scale. Mitigations: a lock per key ("single-flight") or early refresh |

## How can I reproduce/test it?

```powershell
docker compose up -d redis
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
$env:TEST_REDIS_URL   = "redis://localhost:6379/15"
go test -count=1 ./internal/cache/ ./internal/ratelimit/ ./internal/products/ ./tests/ -run "Cache|Allow|Middleware|Breaker|FailsFast|RedisAPI|KeyFuncs" -v
```

Look inside Redis:

```powershell
docker compose exec redis redis-cli --scan --pattern "product:*"
docker compose exec redis redis-cli GET product:v1:2
docker compose exec redis redis-cli TTL product:v1:2
docker compose exec redis redis-cli --scan --pattern "rl:*"
```

Watch rate-limit headers, then hit the limit:

```powershell
$r = Invoke-WebRequest http://localhost:8080/api/v1/products -UseBasicParsing
$r.Headers["X-RateLimit-Limit"]; $r.Headers["X-RateLimit-Remaining"]
1..110 | ForEach-Object { try { Invoke-WebRequest http://localhost:8080/api/v1/products -UseBasicParsing | Out-Null; "ok" } catch { $_.Exception.Response.StatusCode.value__ } } | Group-Object
```

The Redis outage drill:

```powershell
docker compose stop redis
Invoke-RestMethod http://localhost:8080/ready                          # degraded
Measure-Command { Invoke-RestMethod http://localhost:8080/api/v1/products/2 }   # milliseconds
docker compose start redis
```

> **The concurrency demo and rate limiting.** `go run ./cmd/concurrency-demo` logs in 100+ buyers
> from one IP, which is exactly what a rate limiter should stop, so with the default 100/min it
> says so and exits. For demos, raise the limit:
> `$env:RATE_LIMIT_PER_MINUTE = "5000"; docker compose up -d api`, and afterwards
> `Remove-Item Env:RATE_LIMIT_PER_MINUTE; docker compose up -d api`.

### Experiments to try

1. **Recreate the outage.** In `cache/redis.go`, remove `rdb.AddHook(newBreaker(...))` and the `DialerRetries`/`ContextTimeoutEnabled` lines. Rebuild, run `docker compose stop redis`, and time a product request. Then put it all back.
2. **See the stale-read window.** Set `PRODUCT_CACHE_TTL=2m`. GET a product (cached), change its price directly in psql (bypassing the API, so there's no invalidation), and GET again: you get the old price. Wait for the TTL, or PATCH through the API, and the new price appears.
3. **Fail closed instead.** In `ratelimit.Middleware`, return 503 instead of calling `next` when `Allow` errors. Stop Redis, and the entire API now fails. That's the trade-off you'd accept only when rate limiting is a hard security requirement.
4. **Boundary burst.** With `RATE_LIMIT_PER_MINUTE=5`, send 5 requests at hh:mm:59 and 5 at hh:mm+1:00. All 10 succeed within 2 seconds, which is the fixed-window trade-off.
