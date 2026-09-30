# 02 — Authentication & Authorization

## What problem does this solve?

The API has to answer two questions for every protected request:

1. **Authentication: who are you?** Answered by logging in with email and password, which gives the client a token.
2. **Authorization: are you allowed to do this?** Answered by the role in that token (`CUSTOMER` or `ADMIN`) and, from Stage 5, by ownership ("is this *your* order?").

Along the way we must never store readable passwords, never reveal which emails are registered,
and never let two accounts share one email.

## How does our implementation work?

```text
internal/identity   Principal{UserID, Role} stored in context.Context
internal/users      User model, Repository (SQL), GET /users/me handler
internal/auth       PasswordHasher (bcrypt), TokenManager (JWT), Service (register/login),
                    Handler (HTTP), RequireAuth / RequireRole middleware
internal/validate   Collects per-field validation errors
internal/app        Wires it all together
```

The layers, and what each one is allowed to know:

```text
auth.Handler   HTTP only: decode JSON -> call service -> map errors to status codes
auth.Service   Rules: validation, hashing, "unknown email == wrong password", token issuing
UserStore      Interface: Create / GetByEmail (implemented by users.Repository)
users.Repository  SQL only. Converts pgx errors into users.ErrNotFound / users.ErrEmailTaken
```

`auth.Service` depends on the small `UserStore` **interface**, not on `*users.Repository`.
That is the one place an interface pays off here: `service_test.go` swaps in an in-memory fake,
so every business rule is tested in milliseconds without a database.

### Passwords: bcrypt

```text
register:  "super-secret-1"  --bcrypt(cost 10, random salt)-->  $2a$10$N9qo8uLOickgx2ZMRZoMye...
login:     bcrypt.CompareHashAndPassword(storedHash, typedPassword)
```

- **Salted**: the same password gives a different hash each time, so leaked hashes can't be matched against precomputed tables.
- **Slow on purpose**: cost 10 is about 90 ms per hash (look at `duration_ms` on `/auth/login` in the logs). That's nothing for one login, but it turns billions of guesses per second into thousands.
- **72-byte limit**: bcrypt ignores everything after 72 bytes, so we reject longer passwords instead of silently truncating them.

### Tokens: JWT (HS256)

```text
eyJhbGciOiJIUzI1NiIs...  .  eyJyb2xlIjoiQ1VTVE9NRVIiLCJzdWIiOiIxIi...  .  kX9f3...
       header                          payload (claims)                      signature
 {"alg":"HS256"}          {"role":"CUSTOMER","sub":"1","iss":"...",         HMAC-SHA256(
                           "exp":1790760000,"iat":1790756400}               header.payload,
                                                                             JWT_SECRET)
```

- The payload is **only base64, not encrypted**. Paste a token into jwt.io and you can read it, so never put secrets in claims.
- The signature proves the server made the token and nobody edited it. Change `CUSTOMER` to `ADMIN` in the payload and the signature no longer matches (see `TestToken_TamperedPayload`).
- **Stateless**: verification needs only the secret, not a database lookup, so it's fast and scales easily.
- Verification is strict:
  - only `HS256` is accepted, which blocks the classic `"alg":"none"` attack;
  - the issuer must match;
  - `exp` must be present and in the future;
  - `sub` must be a positive number;
  - the role must be known.

### Middleware

```text
/api/v1/users/me:  RequestID -> Logger -> Recoverer -> RequireAuth -> users.Handler.Me
```

- `RequireAuth` reads `Authorization: Bearer <token>`, verifies it, and puts `identity.Principal` into the context. On failure it returns **401 `UNAUTHENTICATED`** with a `WWW-Authenticate: Bearer` header.
- `RequireRole(identity.RoleAdmin)` (used from Stage 3) returns **403 `FORBIDDEN`** when the role doesn't match.
  - **401** means "I don't know who you are".
  - **403** means "I know who you are, and the answer is no".

Once a request is authenticated, every log line written with its context automatically includes
`user_id`, because the logging handler reads it from `identity`.

## What happens during a normal request?

**Register**: `POST /api/v1/auth/register {"email","name","password"}`

1. `httpx.DecodeJSON` rejects an empty body, bad JSON, unknown fields (for example `"role":"ADMIN"`), or a body over 1 MB.
2. `Service.Register` trims the input and validates it, collecting *all* field problems.
3. bcrypt hashes the password.
4. `Repository.Create` runs `INSERT ... RETURNING` with role `CUSTOMER` hard-coded.
5. The API returns `201` with `{id, email, name, role, created_at}`. There is no password hash in the response: `users.User` has no JSON tags, and only `users.Response` is ever encoded.

**Login**: `POST /api/v1/auth/login {"email","password"}`

1. `GetByEmail` looks the user up. The `CITEXT` column makes the match case-insensitive.
2. bcrypt compares the password.
3. `TokenManager.Issue` signs `{sub, role, iss, iat, exp}`.
4. The API returns `200 {"access_token","token_type":"Bearer","expires_in":3599,"expires_at","user"}`.

**Me**: `GET /api/v1/users/me` with `Authorization: Bearer <token>`

1. `RequireAuth` verifies the token and stores `Principal{UserID: 1, Role: CUSTOMER}`.
2. `Me` loads user 1 from the database (the token only carries the id and role) and returns it.

## What happens when it fails?

| Situation | Result |
|---|---|
| Missing or invalid fields | 400 `VALIDATION_ERROR` with `fields: {email:..., password:...}` |
| Unknown JSON field (`"role":"ADMIN"`) | 400: clients cannot choose their own role |
| Email already registered (any letter case) | 409 `EMAIL_ALREADY_EXISTS` |
| Wrong password **or** unknown email | 401 `INVALID_CREDENTIALS`, with an identical body in both cases |
| No, malformed, expired, forged, or `alg:none` token | 401 `UNAUTHENTICATED` |
| Valid token, but the user was deleted | 401 `UNAUTHENTICATED` "user no longer exists" |
| Customer calls an admin route (Stage 3+) | 403 `FORBIDDEN` |
| Database down during login | 500 `INTERNAL_ERROR`, never 401. The real error is only in the logs, tagged with the request_id |
| `JWT_SECRET` missing or shorter than 32 characters | The API refuses to start |

### Preventing user enumeration

If "unknown email" returned a different response from "wrong password", an attacker could test
a list of emails and learn which ones have accounts. We prevent that in two ways:

1. Both cases return the same code and message.
2. **Timing**: for an unknown email, we still run a bcrypt comparison against a dummy hash (`Service.dummyHash`). Otherwise the unknown-email response would come back roughly 90 ms faster, and that timing difference alone would leak the answer.

## What database concepts are involved?

`migrations/000002_create_users.sql`:

| Constraint | Why |
|---|---|
| `id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY` | The database generates ids. `ALWAYS` rejects ids supplied by the client |
| `email CITEXT` + `UNIQUE (email)` named `users_email_key` | One account per mailbox, ignoring case. The unique index also makes login lookups fast |
| `CHECK (role IN ('CUSTOMER','ADMIN'))` | A typo or bug can never create an unknown role |
| `CHECK (btrim(name) <> '')`, `CHECK (password_hash <> '')`, email length | Last line of defence for code paths that skip Go validation (scripts, psql) |
| `NOT NULL` everywhere | No "missing" values to special-case in Go |

`database.IsUniqueViolation(err, "users_email_key")` recognises PostgreSQL error code `23505` for
this exact constraint. That's how the repository turns a database error into
`users.ErrEmailTaken`, without leaking SQL details to the client.

## What concurrency issues exist?

**Two registrations for the same email at the same moment.** A naive design would do this:

```text
A: SELECT ... WHERE email='x'   -> none
B: SELECT ... WHERE email='x'   -> none
A: INSERT 'x'                   -> ok
B: INSERT 'x'                   -> ok ❌ two accounts
```

We never do check-then-insert. We just `INSERT`, and the `UNIQUE` index lets exactly one of the
two inserts succeed. The other fails with `23505`, which becomes `409`.
`TestRepository_ConcurrentRegistrationSameEmail` fires 20 simultaneous inserts and expects exactly
1 success and 19 `ErrEmailTaken`. The same idea ("let a constraint be the referee") is how
idempotency keys work in Stage 8.

**Stale roles in tokens.** If an admin is demoted, their existing token still says `ADMIN` until
it expires (1 hour by default). That's the trade-off of stateless tokens. Short TTLs limit the
window; a real system adds refresh tokens or a revocation list (see "Future improvements" in Stage 15).

## How can I reproduce/test it?

```powershell
# Tests
go test ./internal/auth/ -v                          # unit tests, no database
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 ./internal/users/ ./tests/ -v       # PostgreSQL + full HTTP flow

# Manual (API running via docker compose)
$base = "http://localhost:8080/api/v1"
$body = @{ email = "alice@example.com"; name = "Alice"; password = "super-secret-1" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "$base/auth/register" -ContentType "application/json" -Body $body
$login = Invoke-RestMethod -Method Post -Uri "$base/auth/login" -ContentType "application/json" `
  -Body (@{ email = "ALICE@example.com"; password = "super-secret-1" } | ConvertTo-Json)
Invoke-RestMethod -Uri "$base/users/me" -Headers @{ Authorization = "Bearer $($login.access_token)" }

# Look inside the stored data: only a bcrypt hash, never the password
docker compose exec postgres psql -U app -d inventory -c "SELECT id, email, role, password_hash FROM users;"
```

### Experiments to try

1. **Decode your token.** Split `$login.access_token` on `.`, then base64-decode the middle part. You'll see `sub`, `role`, and `exp` in plain text.
2. **Forge a role.** Edit the payload to say `"ADMIN"`, re-encode it, and call `/users/me`. You get 401, because the signature no longer matches.
3. **Break the rule.** In `users/repository.go`, add a `GetByEmail` check before the `INSERT` and remove the unique-violation handling. Then drop the constraint in a new migration and run `TestRepository_ConcurrentRegistrationSameEmail`. You should see duplicate accounts get through, which is exactly the race described above.
4. **Measure the timing defence.** Temporarily delete the `dummyHash` comparison in `Service.Login`. Then compare `duration_ms` in the logs for an unknown email versus a wrong password.
5. **Expire a token quickly.** Restart the API with `JWT_TTL=30s` (`$env:JWT_TTL="30s"` when using `go run`), log in, wait 31 seconds, and call `/users/me`.
