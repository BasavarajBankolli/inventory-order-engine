-- 000008_create_payments.sql
-- One payment attempt per order.
--
--   PENDING    row written BEFORE the provider is called; also the state
--              when the provider timed out and the outcome is unknown
--   SUCCEEDED  money taken; provider_reference identifies the charge
--   FAILED     declined; the order was cancelled and its stock released

CREATE TABLE payments (
    id                 BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id           BIGINT      NOT NULL REFERENCES orders (id),
    amount             BIGINT      NOT NULL,
    currency           TEXT        NOT NULL,
    status             TEXT        NOT NULL DEFAULT 'PENDING',
    provider_reference TEXT,
    failure_reason     TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- ONE payment per order. A retry after a timeout reuses this row (and
    -- its provider idempotency key) instead of starting a second charge.
    CONSTRAINT payments_order_id_key UNIQUE (order_id),

    -- The provider's charge id can belong to only one payment.
    CONSTRAINT payments_provider_reference_key UNIQUE (provider_reference),

    CONSTRAINT payments_amount_check   CHECK (amount > 0),
    CONSTRAINT payments_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payments_status_check   CHECK (status IN ('PENDING', 'SUCCEEDED', 'FAILED')),

    -- A successful payment must say WHICH charge took the money, otherwise
    -- nobody could ever find it at the provider (refunds, disputes).
    CONSTRAINT payments_succeeded_has_reference_check
        CHECK (status <> 'SUCCEEDED' OR provider_reference IS NOT NULL)
);
