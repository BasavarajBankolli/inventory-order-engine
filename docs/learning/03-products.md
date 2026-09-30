# 03 — Products

## What problem does this solve?

Before anyone can order, the system needs a **catalogue**: what exists, under which SKU, and at
what price. Admins manage it. Anyone can browse it, with search, filtering, sorting and
pagination.

This stage also introduces three ideas that every later module reuses:

1. **Money as integers.** Prices are stored in minor units, never as floats.
2. **Soft delete.** "Deleting" a product archives it instead of removing the row.
3. **Safe dynamic SQL.** Search, filters and sorting are built without any risk of SQL injection.

## Why do we need it?

- Orders (Stage 5) copy the product's price at the moment of purchase. That price must be exact.
- Orders reference products by id. Removing a product row would break order history, or the foreign key would refuse the delete.
- A list endpoint that returns *everything* works with 10 products and falls over with 100,000. Pagination keeps each response small.

## How does our implementation work?

```text
products.Handler     parse path/query/JSON, map errors to HTTP     (handler.go)
products.Service     normalise + validate, "delete" = archive      (service.go)
products.Repository  all SQL for the products table                (repository.go)
```

### Money: integers in minor units

```text
"price": 129900, "currency": "INR"   ->  ₹1,299.00   (100 paise per rupee)
"price":   1999, "currency": "USD"   ->  $19.99      (100 cents per dollar)
```

Why not `float64`?

```go
fmt.Println(0.1 + 0.2)   // 0.30000000000000004
```

Floats store binary fractions, and most decimal amounts can't be represented exactly. Add up
thousands of order lines and the totals drift. Integers are exact, and `BIGINT` holds values up
to 9.2 × 10¹⁸. We cap a price at 1,000,000,000 minor units (₹1 crore). That way
`price × quantity` in an order can never overflow.

JSON `"price": 12.5` is rejected with 400, because Go refuses to decode `12.5` into an `int64`.

### Status and soft delete

| Status | Visible in `GET /products`? | `GET /products/{id}` | Can PATCH? | How to reach it |
|---|---|---|---|---|
| `ACTIVE` | yes (the default filter) | 200 | yes | create or PATCH |
| `INACTIVE` | only with `?status=INACTIVE` | 200 | yes | create or PATCH |
| `ARCHIVED` | never | 404 | no (404) | `DELETE` only |

`DELETE /products/{id}` runs `UPDATE products SET status='ARCHIVED'`. The row, and its SKU,
stay forever so that old orders keep pointing at a real product.

### Partial updates (PATCH) with pointers + COALESCE

```go
type updateRequest struct {
    Name        *string `json:"name"`         // absent from JSON  -> nil
    Description *string `json:"description"`  // "description": "" -> pointer to ""
    Price       *int64  `json:"price"`
}
```

```sql
UPDATE products SET
    name        = COALESCE($2, name),         -- nil pointer -> NULL -> keep the old value
    description = COALESCE($3, description),
    price       = COALESCE($4, price),
    updated_at  = now()
WHERE id = $1 AND status <> 'ARCHIVED'
RETURNING ...
```

One fixed SQL statement handles every combination of fields. The pointers are what let us tell
"not sent" (`nil`) apart from "set to empty" (`""`).

### List: search, filter, sort, paginate, safely

`GET /api/v1/products?q=shirt&status=ACTIVE&sort=price_asc&limit=20&offset=40`

```sql
SELECT count(*) FROM products WHERE status = $1 AND (name ILIKE $2 OR sku ILIKE $2);
SELECT ... FROM products WHERE status = $1 AND (name ILIKE $2 OR sku ILIKE $2)
ORDER BY price ASC, id ASC LIMIT $3 OFFSET $4;
```

The rules that make this safe:

| Part | How it is kept safe |
|---|---|
| `q`, `status`, `limit`, `offset` | Always passed as `$n` parameters, never pasted into the SQL text |
| `sort` | `ORDER BY` can't take parameters, so `?sort=` must match a key in the `sortOrders` map. Only our own fixed strings ever reach the SQL; anything else is a 400 |
| `%` and `_` in `q` | `escapeLike` turns them into `\%` and `\_`, so searching "50%" matches the literal text "50%" instead of treating `%` as "anything" |
| Tie-breaking | Every sort ends with `id`. Without that, rows with equal prices could swap places between pages, so items could repeat or be skipped |

The response includes `total` so a client can show "page 3 of 7":

```json
{ "items": [ ... ], "total": 134, "limit": 20, "offset": 40 }
```

## What happens during a normal request?

`POST /api/v1/products` with an admin token and body `{"sku":"mouse-01","name":" Wireless Mouse ","price":129900,"currency":"inr"}`

1. `RequireAuth` verifies the token, and `RequireRole(ADMIN)` checks the role. A customer gets 403 here.
2. `DecodeJSON` decodes the body strictly: unknown fields are rejected, and a float price is rejected.
3. `normalizeCreate` upper-cases the SKU and currency, trims the name, and defaults the status to `ACTIVE`.
4. `validateCreate` checks every field and reports all problems together.
5. `Repository.Create` runs `INSERT ... RETURNING`. A duplicate SKU becomes `ErrSKUTaken`.
6. The response is `201 Created`, with `Location: /api/v1/products/2` and the product JSON.

## What happens when it fails?

| Situation | Result |
|---|---|
| No token / customer token on POST, PATCH or DELETE | 401 / 403 |
| Invalid fields (price ≤ 0, bad currency, bad SKU, name too long) | 400 `VALIDATION_ERROR` with `fields` |
| `"price": 12.5` | 400 `field "price" has the wrong type` |
| `sku` in a PATCH body | 400 unknown field (SKUs are permanent) |
| `"status": "ARCHIVED"` in a PATCH | 400: archiving only happens through DELETE |
| Duplicate SKU (any letter case, since input is upper-cased) | 409 `SKU_ALREADY_EXISTS`, including SKUs of archived products |
| `/products/abc`, `/products/0` | 400 `id must be a positive integer` |
| Missing or archived product | 404 `NOT_FOUND` |
| `?limit=500`, `?sort=cheapest`, `?status=ARCHIVED` | 400, with every bad parameter listed |

## What database concepts are involved?

`migrations/000003_create_products.sql`:

| Constraint / index | Why |
|---|---|
| `UNIQUE (sku)` named `products_sku_key` | Two products can never share a SKU. The repository recognises this constraint to return 409 |
| `CHECK (sku ~ '^[A-Z0-9][A-Z0-9_-]{1,63}$')` | Enforces the SKU format even for rows inserted outside the API |
| `CHECK (price > 0 AND price <= 1000000000)` | No free or negative products, and no overflow in order totals |
| `CHECK (currency ~ '^[A-Z]{3}$')` | ISO-style currency codes only |
| `CHECK (status IN (...))` | Only known lifecycle states |
| `INDEX (status, id)` | Serves the most common query, `WHERE status='ACTIVE' ORDER BY id DESC LIMIT 20`, without sorting the whole table |

Other concepts in this stage:

- **`COALESCE(a, b)`** returns the first non-NULL argument. That's the whole trick behind PATCH.
- **`RETURNING`** gets the inserted or updated row back in the same statement, with no second SELECT.
- **`RowsAffected()`** tells `Archive` whether any row matched. Zero means "not found or already archived", so the API answers 404.
- **`ILIKE`** is case-insensitive `LIKE`. `%text%` can't use a normal B-tree index, which is fine for a small catalogue. At scale you'd add a `pg_trgm` index or a search engine.
- **`OFFSET` pagination** is simple, but the database still has to walk past every skipped row. So `OFFSET 100000` gets slow, and rows inserted while a user is paging can shift the pages. The alternative is **keyset pagination** (`WHERE id < $last_seen_id ORDER BY id DESC LIMIT 20`), listed under future improvements.

## What concurrency issues exist?

- **Two admins create the same SKU at the same moment.** This is the same pattern as registering an email: we never check first and insert second. The `UNIQUE` constraint lets one insert win, and the other gets 409.
- **Two admins PATCH the same product at once.** Each `UPDATE` is atomic, and the last one wins, field by field (COALESCE only touches the fields each request sent). For product details that is acceptable. **Stock quantities are different:** there "last write wins" loses sales. That's why Stage 4 adds a `version` column and row locking to inventory.
- **An admin archives a product while a customer is ordering it.** Stage 5 handles this: order creation re-reads the product inside the order transaction and rejects non-`ACTIVE` products.

## How can I reproduce/test it?

```powershell
# Unit tests (validation, normalisation, LIKE escaping); no database needed
go test ./internal/products/ -run "Validate|Normalize|ListParams|EscapeLike" -v

# Repository and API tests with PostgreSQL
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
go test -count=1 ./internal/products/ ./tests/ -v -run "Repository|Products"
```

Manually (you need an ADMIN token; see the README section "Creating an admin"):

```powershell
$base = "http://localhost:8080/api/v1"
$login = Invoke-RestMethod -Method Post -Uri "$base/auth/login" -ContentType "application/json" `
  -Body (@{ email = "alice@example.com"; password = "super-secret-1" } | ConvertTo-Json)
$admin = @{ Authorization = "Bearer $($login.access_token)" }

Invoke-RestMethod -Method Post -Uri "$base/products" -Headers $admin -ContentType "application/json" `
  -Body (@{ sku = "kb-01"; name = "Keyboard"; price = 249900; currency = "inr" } | ConvertTo-Json)
Invoke-RestMethod -Uri "$base/products?q=key&sort=price_desc"
Invoke-RestMethod -Method Patch -Uri "$base/products/1" -Headers $admin -ContentType "application/json" -Body '{"price": 199900}'
Invoke-WebRequest -Method Delete -Uri "$base/products/1" -Headers $admin -UseBasicParsing
docker compose exec postgres psql -U app -d inventory -c "SELECT id, sku, price, status FROM products;"
```

### Experiments to try

1. **See the float problem.** In a Go playground, sum `0.1` ten times and compare the result with `1.0`. Then do the same with the integer `10` added ten times.
2. **Try SQL injection.** Call `GET /products?q=' OR 1=1 --`. You get an empty list: the text is just a search value. Then change the `?sort=` handling to paste the raw value into `ORDER BY` (don't commit this!) and see how dangerous that would be.
3. **Unescaped LIKE.** Remove `escapeLike` in `List`, create a product named "100% cotton", and search for `%`. Now every product matches.
4. **Unstable pages.** Change `"price_asc": "price ASC, id ASC"` to `"price ASC"`, create 30 products with the same price, and page through them with `limit=10`. PostgreSQL is free to return equal rows in any order, so items can repeat or go missing.
5. **Check the index.** In psql, run `EXPLAIN SELECT * FROM products WHERE status='ACTIVE' ORDER BY id DESC LIMIT 20;`. On a tiny table PostgreSQL may prefer a sequential scan, which is normal. Insert ~50,000 rows with `generate_series` and run it again.
