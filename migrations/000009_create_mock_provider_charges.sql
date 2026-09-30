-- 000009_create_mock_provider_charges.sql
-- Storage for the MOCK payment provider only.
--
-- A real provider (Stripe, Razorpay) keeps its own records on its own
-- servers. Our mock needs the same thing: memory that survives restarts and
-- is shared by every process that talks to "the provider" - the API (which
-- charges) and the worker (which asks "did that charge go through?").
-- An in-memory map cannot do that, because the API and the worker are
-- different processes.
--
-- Think of this table as belonging to the payment provider, not to us:
-- our own code never reads it, only internal/payments/mock.go does.

CREATE TABLE mock_provider_charges (
    -- The provider-side idempotency key: one charge per key, ever.
    idempotency_key TEXT        PRIMARY KEY,
    status          TEXT        NOT NULL,
    reference       TEXT,
    failure_reason  TEXT,
    amount          BIGINT      NOT NULL,
    currency        TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mock_provider_charges_status_check CHECK (status IN ('SUCCEEDED', 'DECLINED'))
);
