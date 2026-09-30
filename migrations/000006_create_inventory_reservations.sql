-- 000006_create_inventory_reservations.sql
-- A reservation holds stock for one product of one order until the order is
-- paid (CONFIRMED), cancelled (RELEASED) or the time runs out (EXPIRED).
--
-- Invariant kept by the application, inside one transaction every time:
--   inventory.reserved_quantity (for a product)
--     = SUM(quantity) of that product's ACTIVE reservations

CREATE TABLE inventory_reservations (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id    BIGINT      NOT NULL REFERENCES orders (id),
    product_id  BIGINT      NOT NULL REFERENCES products (id),
    quantity    INTEGER     NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'ACTIVE',

    -- After this moment an ACTIVE reservation may be expired by the
    -- background worker (Stage 10) and its stock returned to available.
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One reservation per product per order (mirrors order_items).
    -- Its index also serves "all reservations of order X".
    CONSTRAINT inventory_reservations_order_product_key UNIQUE (order_id, product_id),

    CONSTRAINT inventory_reservations_quantity_check CHECK (quantity > 0),
    CONSTRAINT inventory_reservations_status_check
        CHECK (status IN ('ACTIVE', 'CONFIRMED', 'RELEASED', 'EXPIRED')),
    CONSTRAINT inventory_reservations_expiry_check CHECK (expires_at > created_at)
);

-- PARTIAL INDEX: only ACTIVE rows are indexed. The expiry worker asks
-- "which ACTIVE reservations have expires_at < now()?" many times a minute.
-- Finished reservations (the vast majority over time) are never in this
-- index, so it stays small and fast no matter how old the shop gets.
CREATE INDEX inventory_reservations_active_expires_at_idx
    ON inventory_reservations (expires_at)
    WHERE status = 'ACTIVE';
