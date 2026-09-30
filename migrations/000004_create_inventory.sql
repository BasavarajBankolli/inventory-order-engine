-- 000004_create_inventory.sql
-- Stock levels, one row per product.
--
--   available_quantity  units that can still be sold right now
--   reserved_quantity   units held for orders that are not finished yet
--                       (used from Stage 6: reserve -> confirm/release)
--
-- Both are counts of physical units, so they are plain integers.

CREATE TABLE inventory (
    id                 BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- FOREIGN KEY: an inventory row can only exist for a real product, and
    -- a product that has inventory cannot be physically deleted (the default
    -- ON DELETE behaviour is to refuse). Products are archived instead.
    product_id         BIGINT      NOT NULL REFERENCES products (id),

    available_quantity INTEGER     NOT NULL DEFAULT 0,
    reserved_quantity  INTEGER     NOT NULL DEFAULT 0,

    -- Incremented on every change. Clients send back the version they read
    -- to prove they are not overwriting a newer change (optimistic locking).
    version            BIGINT      NOT NULL DEFAULT 1,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Exactly one inventory row per product. Also indexes product_id, which
    -- is how every inventory query looks rows up.
    CONSTRAINT inventory_product_id_key UNIQUE (product_id),

    -- THE core invariant of this project: stock can never go negative.
    -- Even if application code had a bug, PostgreSQL would reject the
    -- UPDATE and roll back the transaction instead of storing -1.
    CONSTRAINT inventory_available_nonnegative_check CHECK (available_quantity >= 0),
    CONSTRAINT inventory_reserved_nonnegative_check  CHECK (reserved_quantity >= 0),

    -- Upper bound keeps available + reserved far from INTEGER overflow.
    CONSTRAINT inventory_quantity_max_check
        CHECK (available_quantity <= 1000000000 AND reserved_quantity <= 1000000000)
);

-- Products created before this migration get an empty inventory row, so
-- the rule "every product has exactly one inventory row" holds for old
-- data too. New products get theirs in the same transaction that creates
-- the product (products.Service.Create).
INSERT INTO inventory (product_id)
SELECT id FROM products;
