# 13 — Testing Strategy

## What problem does this solve?

A backend that "works on my machine" is easy. A backend you can **prove** is correct under
concurrency, partial failures and outages is the point of this project. Tests are that proof,
and they let you change code later without fear.

## Why do we need it?

Most bugs this project could have don't show up in a simple manual test:
- two buyers at the same instant;
- a connection dying mid-transaction;
- Redis hanging instead of refusing.

Only tests that deliberately create those situations, repeatedly, catch them. The table at the
end of this doc lists **real bugs these tests found while the project was built**.

## How does our implementation work?

### The layers

| Layer | Where | Needs | Example |
|---|---|---|---|
| **Unit**: pure logic, microseconds | `internal/*/..._test.go` (package-internal) | nothing | `TestCanTransition_AllPairs`, `TestBuildItems_*`, `TestAdjust` |
| **Integration**: real PostgreSQL / Redis | `internal/*/..._test.go` | `TEST_DATABASE_URL`, `TEST_REDIS_URL` | `TestRepository_*`, `TestConcurrency_*`, `TestFailure_*` |
| **End-to-end API**: real HTTP through router, middleware, services, DB | `tests/` | both | `TestAPI_100ConcurrentBuyersForTheLastUnit`, `TestDatabaseDown_APIFailsCleanly` |
| **Live drills**: the real Docker stack | manual, documented per stage | Docker | stop Redis or PostgreSQL mid-traffic; `cmd/concurrency-demo` |

Design choices that keep these tests trustworthy:

- **Real databases, no mocks for SQL.** A mocked repository can't prove that a `FOR UPDATE`, a `UNIQUE` index or a `CHECK` constraint actually works. Interfaces are used only where they add value (`auth.UserStore`, `payments.Provider`, `events.Publisher`, `products.Cache`).
- **Isolation.** Every integration test gets its own empty, migrated PostgreSQL **schema** (`testutil.NewMigratedPool`) and its own Redis **key prefix** (`testutil.NewRedis`). Test packages run in parallel without seeing each other's data.
- **Tests that exercise production wiring.** API tests build the app with `app.NewHandler`, the same function `cmd/api` uses.
- **A "starting gun" for races.** Concurrency tests park N goroutines on a channel and `close()` it, so they really do hit the database at the same moment.
- **Integration tests skip without their env vars,** so `go test ./...` always works. A skip proves nothing, though, so `scripts/test.ps1` counts and prints skips.

### Running everything: `scripts/test.ps1`

```powershell
.\scripts\test.ps1                                # everything, once
.\scripts\test.ps1 -Race -Coverage                # + race detector + merged coverage + coverage.html
.\scripts\test.ps1 -Run Concurrency -Count 20 -Race   # hammer the concurrency tests
```

It:
1. starts PostgreSQL and Redis and waits until they're healthy;
2. sets `TEST_DATABASE_URL` and `TEST_REDIS_URL`;
3. runs `gofmt` and `go vet`;
4. runs `go test`, then prints passed/failed/**skipped**;
5. optionally merges coverage across all test binaries.

Result at the end of this stage: **359 tests passed, 0 failed, 0 skipped. Coverage 88.7%.**

### Measuring coverage correctly (two traps we fell into)

1. **Cross-package coverage.** Most handler code is exercised by the API tests in `tests/`, not by the handler's own package. Plain `go test -cover` only counts a package's own tests. You need `-coverpkg=./internal/...`, and the per-binary results must be merged with `GOCOVERDIR` + `go tool covdata`. A single `-coverprofile` across many packages gave misleading numbers (`identity` at 0%, though every API test uses it).
2. **PowerShell splits arguments.** Unquoted, `-coverpkg=./internal/...` is split at the `.` into two arguments. Go then silently ran without cross-package coverage, and the report claimed **no test ever called `auth.Register`**. Always quote such arguments in PowerShell. The script does.

### Requirement → test map (from the project brief)

| Requirement | Tests |
|---|---|
| **Unit:** order total calculation | `TestBuildItems_TotalsAndSnapshot`, `TestBuildItems_LargestPossibleOrderDoesNotOverflow` |
| **Unit:** inventory validation | `TestAdjust`, `TestSetAvailable`, `TestValidateUpdate` (inventory), `TestReserve`, `TestReleaseReserved`, `TestConfirmReserved` |
| **Unit:** order state transitions | `TestCanTransition_AllPairs` (all 100 pairs), `TestTransitionTo*`, `TestIsFinal` |
| **Unit:** reservation logic | `TestReserveReleaseKeepsTotal`, reservation tests above |
| **Unit:** idempotency logic | `TestFingerprint`, `TestValidateIdempotencyKey` |
| **Integration:** API → PostgreSQL | everything in `tests/` (auth, products, inventory, orders, payments, idempotency, redis) |
| **Integration:** transaction behaviour | `TestWithTx`, `TestCreate_RejectsAndWritesNothing`, `TestReserve_OutOfStockRollsBackEverything`, `TestAdd_IsPartOfTheTransaction` |
| **Concurrency:** 100 users, 1 stock | `TestConcurrency_100BuyersOneItem` (service), `TestAPI_100ConcurrentBuyersForTheLastUnit` (HTTP), live `cmd/concurrency-demo` |
| **Concurrency:** more | no lost updates, never negative, optimistic locking, no deadlocks, create/cancel storm, archive during orders, concurrent cancels, concurrent pay clicks, concurrent idempotent duplicates, concurrent workers (expiry + outbox), rate-limit counter |
| **Failure:** payment failure | `TestPay_FailureReleasesStock`, `TestPayments_API_SuccessFailureTimeout` |
| **Failure:** payment timeout | `TestPay_TimeoutThenRetryChargesOnce`, `TestMock_TimeoutThenRetryAndStatus`, `TestReconcile_ChargedTimeoutIsConfirmed` |
| **Failure:** database failure | `TestDatabaseDown_APIFailsCleanly`, `TestFailure_ConnectionKilledMidTransaction`, `TestFailure_ClientCancelsWhileWaiting`, `TestFailure_ErrorHalfwayRollsBackEarlierSteps`, `TestConnect_*`, `TestWorkerJobs_DatabaseDownReturnsErrors`, `TestReady` |
| **Failure:** Redis failure | `TestRedisAPI_WorksWhenRedisIsDown`, `TestRedisClient_FailsFastWhenRedisHangs`, `TestCache_RedisDownFallsBackToDatabase`, `TestMiddleware_FailsOpenWhenRedisIsDown` |
| **Failure:** expired reservation | `TestExpire_*`, `TestReconcile_*`, `TestPay_Rejections` (expired → no charge) |
| **Failure:** duplicate request | `TestIdempotency_*` (incl. 20 concurrent duplicates), `TestRegister_DuplicateEmailIsCaseInsensitive`, duplicate SKU tests |
| **Failure:** worker retry | `TestProcessBatch_RetryWithBackoffThenDeadLetter`, `TestProcessBatch_RetrySucceedsLater`, `TestRun_RepeatsJobsAndStopsOnCancel`, `TestReconcile_ProviderUnreachableChangesNothing` |

### New in this stage: database failure mid-flight

| Test | Scenario | Guarantee |
|---|---|---|
| `TestDatabaseDown_APIFailsCleanly` | PostgreSQL unreachable, 9 endpoints | JSON 500 with a request_id, no host/driver/password in the message, no hang. `/health` 200, `/ready` 503 |
| `TestFailure_ConnectionKilledMidTransaction` | `pg_terminate_backend` while the order transaction waits for a lock | Order row, items, reservations and events all gone. The pool recovers, and the next order works |
| `TestFailure_ClientCancelsWhileWaiting` | HTTP client gives up while its transaction waits | Nothing written, and **the server session stops waiting** (see bug #8 below) |
| `TestFailure_ErrorHalfwayRollsBackEarlierSteps` | Step 2 of cancel fails (inconsistent data) | Step 1 (status change) is rolled back too, and no `OrderCancelled` event |

Live drill: with PostgreSQL stopped, `/ready` gives 503, requests get JSON 500s, and the worker
logs `job failed` and keeps running. After `docker compose start postgres`, everything recovers
**without restarting the API or the worker**.

## What happens when it fails?

A failing test prints what it expected and what it got, for example
`ok=3, want all 60 (unexpected errors: 57, first: ... deadlock detected (SQLSTATE 40P01))`.
Concurrency tests summarise errors (count plus the first one) instead of dumping hundreds.

**A test must be able to fail.** For every important safety mechanism we removed it on a
throwaway copy of the code and checked that the test catches it:

| Mechanism removed | Test result |
|---|---|
| `FOR UPDATE` + version check (inventory) | 50 "successful" restocks → only 6 units recorded |
| Product-id lock ordering | 50–57 of 60 orders died with `deadlock detected` |
| Unique index on idempotency keys | 16–19 duplicate orders and 32–38 units reserved (instead of 1 and 2) |
| `SKIP LOCKED` in the outbox | every event published 3 times |

## What database concepts are involved?

- **Schema-per-test isolation** with `search_path`.
- **`pg_stat_activity` / `pg_blocking_pids`** to find exactly *our* blocked session, never another parallel test's.
- **`pg_terminate_backend`** to simulate a crash or network cut.
- **Server-side query cancellation** (`CancelRequestContextWatcherHandler`).
- **Race detector** (`-race`) for data races in Go code. (Database races are what the concurrency tests catch.)

## What concurrency issues exist?

Concurrency tests are only trustworthy when they're **repeated**. They are run with `-count=10`
or `-count=20` under `-race` at each stage (for example 120/120, 40/40, 30/30). A test that fails
1 time in 50 is telling you something real, usually about the test's own assumptions (see bugs
#7 and #9).

## Bugs the tests (and drills) found while building this project

| # | Stage | Bug | Found by |
|---|---|---|---|
| 1 | 3 | A raw `;` in a URL query silently drops the parameter in Go, so an "SQL injection" test wasn't testing anything | API test that expected a 400 |
| 2 | 4 | A learning-doc experiment claimed the wrong outcome | Running the experiment on a throwaway copy |
| 3 | 6 | One-letter test SKUs violated the SKU `CHECK` constraint | The constraint itself |
| 4 | 10 | The in-memory mock provider was invisible to the separate worker process, so the worker would have **expired paid orders** | Design review for reconciliation |
| 5 | 11 | Redis stopped in Docker → **every request took 65–98 s** (DNS hang + 5 dial retries + ignored deadlines) | Live drill (the unit test used a closed port, which fails fast) |
| 6 | 11 | A rate-limit exact-count test flaked only under full-suite load (timeouts + retried `INCR` counted twice) | Full-suite run with `-race` |
| 7 | 13 | The coverage report claimed handlers were untested | Suspicious numbers → PowerShell argument splitting |
| 8 | 13 | **Cancelled requests left zombie PostgreSQL sessions** waiting on locks (pgx default doesn't cancel server-side) | `TestFailure_ClientCancelsWhileWaiting` (after making its check precise) |
| 9 | 13 | A test helper that would have killed *another package's* database session | Code review of the helper (`pg_blocking_pids` fix) |
| 10 | 13 | A failing test hung the whole run for 10+ minutes (lock holder never rolled back, so the pool waited forever) | The hang itself → `t.Cleanup` for the rollback |

Lessons:
- **Test the failure you'll really get**: hanging, not just refusing (#5).
- **Make checks precise** (#8, #9).
- **Every resource a test opens must be closed on every path**, including `t.Fatal` (#10).
- **Distrust good-looking numbers you didn't expect** (#7).

## How can I reproduce/test it?

```powershell
.\scripts\test.ps1 -Race -Coverage          # the full proof, ~2-3 minutes
start coverage.html                          # browse uncovered lines (red)
.\scripts\test.ps1 -Run "Failure_|DatabaseDown" -Count 5
```

Database outage drill:

```powershell
docker compose stop postgres
Invoke-RestMethod http://localhost:8080/health          # 200
try { Invoke-RestMethod http://localhost:8080/ready } catch { $_.ErrorDetails.Message }   # 503
docker compose logs worker --since 30s                  # "job failed", still running
docker compose start postgres                           # recovers by itself
```

### Experiments to try

1. **Break a guarantee, watch a test catch it.** Pick a row from the "mechanism removed" table, make the change, and run the named test. Then revert.
2. **Find untested code.** Open `coverage.html` and look for red lines in `internal/orders/`. Write a test that turns one green.
3. **Make a test flaky on purpose.** In `TestConcurrency_100BuyersOneItem`, remove the "starting gun" (let goroutines start immediately). It still passes, because the lock is correct, but the race is now weaker. Run it with `-count=50` and think about why a weak race test is dangerous: it can pass even when the locking is broken.
4. **Re-create bug #8.** In `database.CancelQueriesOnContextDone`, return the default `DeadlineContextWatcherHandler` instead, and run `TestFailure_ClientCancelsWhileWaiting`. The server session keeps waiting after the client is gone.
