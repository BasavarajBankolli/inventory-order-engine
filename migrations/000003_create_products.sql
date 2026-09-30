-- 000003_create_products.sql
-- The product catalogue.
--
-- MONEY: price is a BIGINT in MINOR units (paise, cents), never a float.
--   price = 129900, currency = 'INR'  ->  Rs 1,299.00
--   price =   1999, currency = 'USD'  ->  $19.99
-- Floats cannot represent most decimals exactly (0.1 + 0.2 = 0.30000000000000004),
-- which slowly corrupts totals. Integers are exact.

CREATE TABLE products (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sku         TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    price       BIGINT      NOT NULL,
    currency    TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'ACTIVE',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- SKU (Stock Keeping Unit) is the business identifier warehouses and
    -- other systems use. Two products must never share one.
    CONSTRAINT products_sku_key UNIQUE (sku),
    CONSTRAINT products_sku_format_check CHECK (sku ~ '^[A-Z0-9][A-Z0-9_-]{1,63}$'),

    CONSTRAINT products_name_not_blank_check CHECK (btrim(name) <> ''),

    -- Price must be positive. The upper bound (10 million major units)
    -- keeps price * quantity far away from BIGINT overflow in order totals.
    CONSTRAINT products_price_check CHECK (price > 0 AND price <= 1000000000),

    -- ISO 4217 style code: three upper-case letters (INR, USD, EUR).
    CONSTRAINT products_currency_check CHECK (currency ~ '^[A-Z]{3}$'),

    -- ACTIVE   = visible and can be ordered
    -- INACTIVE = temporarily not for sale (admin can switch back)
    -- ARCHIVED = "deleted". We never physically DELETE a product, because
    --            orders (Stage 5) reference it and must keep their history.
    CONSTRAINT products_status_check CHECK (status IN ('ACTIVE', 'INACTIVE', 'ARCHIVED'))
);

-- The product list is filtered by status and ordered newest-first by default.
-- This index serves "WHERE status = 'ACTIVE' ORDER BY id DESC LIMIT 20"
-- without sorting the whole table.
CREATE INDEX products_status_id_idx ON products (status, id);
