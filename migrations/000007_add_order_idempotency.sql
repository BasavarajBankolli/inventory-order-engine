-- 000007_add_order_idempotency.sql
-- Idempotency keys for POST /orders.
--
-- A client sends "Idempotency-Key: <unique value>" with a create-order
-- request. If the request is retried with the same key (e.g. after a network
-- timeout), the API returns the order that was already created instead of
-- creating a second one.

-- Adding NULLable columns without a default is instant in PostgreSQL: only
-- the table's metadata changes, existing rows are not rewritten.
ALTER TABLE orders
    ADD COLUMN idempotency_key TEXT,
    -- SHA-256 of the request body (see orders.fingerprint). Lets us detect a
    -- key being reused for a DIFFERENT request, which is a client bug.
    ADD COLUMN request_hash    TEXT;

ALTER TABLE orders
    ADD CONSTRAINT orders_idempotency_key_length_check
        CHECK (idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 1 AND 255),
    -- Both or neither: a key without a hash could never be checked.
    ADD CONSTRAINT orders_idempotency_hash_check
        CHECK ((idempotency_key IS NULL) = (request_hash IS NULL));

-- THE guarantee: one order per (user, key).
--
-- Why per user? Keys are chosen by clients. Two different customers may
-- well both send "retry-1"; that must not make one of them see the other's
-- order.
--
-- Why the database and not a Go check? Two retries can arrive at the same
-- moment and both find "no order with this key yet". Only a UNIQUE index can
-- let exactly one of their INSERTs win - the other one waits for the
-- winner's transaction and then fails with a unique violation.
--
-- Partial ("WHERE ... IS NOT NULL"): orders placed without a key are not
-- indexed at all, which keeps the index small.
--
-- Note: on a big production table you would use CREATE UNIQUE INDEX
-- CONCURRENTLY (outside a transaction) so writes are not blocked while the
-- index is built. Our migration runner wraps each file in a transaction,
-- which is fine at this project's size.
CREATE UNIQUE INDEX orders_user_idempotency_key_idx
    ON orders (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
