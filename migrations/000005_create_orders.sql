-- 000005_create_orders.sql
-- Orders and their line items.

CREATE TABLE orders (
    id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- Who placed the order. The FK guarantees the user exists and stops a
    -- user with orders from being physically deleted.
    user_id      BIGINT      NOT NULL REFERENCES users (id),

    status       TEXT        NOT NULL DEFAULT 'CREATED',

    -- Sum of order_items.total_price, in minor units (like product prices).
    total_amount BIGINT      NOT NULL,
    currency     TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Every state of the order state machine (internal/orders/status.go).
    -- The Go code decides which TRANSITIONS are legal; the database makes
    -- sure no unknown state is ever stored.
    CONSTRAINT orders_status_check CHECK (status IN (
        'CREATED', 'RESERVED', 'PAYMENT_PENDING', 'PAYMENT_FAILED', 'CONFIRMED',
        'PROCESSING', 'SHIPPED', 'DELIVERED', 'CANCELLED', 'EXPIRED')),
    CONSTRAINT orders_total_amount_check CHECK (total_amount > 0),
    CONSTRAINT orders_currency_check CHECK (currency ~ '^[A-Z]{3}$')
);

-- "My orders, newest first": WHERE user_id = $1 ORDER BY id DESC LIMIT 20.
-- The index returns them already sorted, without scanning other users' orders.
CREATE INDEX orders_user_id_id_idx ON orders (user_id, id DESC);

CREATE TABLE order_items (
    id          BIGINT  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id    BIGINT  NOT NULL REFERENCES orders (id),
    product_id  BIGINT  NOT NULL REFERENCES products (id),
    quantity    INTEGER NOT NULL,

    -- PRICE SNAPSHOT: the product's price at the moment of ordering. If the
    -- admin changes the product price tomorrow, this order must not change.
    unit_price  BIGINT  NOT NULL,
    total_price BIGINT  NOT NULL,

    -- A product appears at most once per order (quantity says how many).
    -- The index also serves "all items of order X" (order_id is its first column).
    CONSTRAINT order_items_order_product_key UNIQUE (order_id, product_id),

    CONSTRAINT order_items_quantity_check    CHECK (quantity > 0),
    CONSTRAINT order_items_unit_price_check  CHECK (unit_price > 0),
    -- The line total can never disagree with its own unit price x quantity.
    CONSTRAINT order_items_total_price_check CHECK (total_price = unit_price * quantity)
);
