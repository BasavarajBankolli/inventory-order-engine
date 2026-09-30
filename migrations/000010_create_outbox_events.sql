-- 000010_create_outbox_events.sql
-- The TRANSACTIONAL OUTBOX.
--
-- Business code INSERTs an event row in the SAME transaction as the change
-- it describes (order created, order confirmed, ...). A background job later
-- publishes the rows to the outside world and marks them PROCESSED.
-- Either the change AND its event are committed, or neither is.

CREATE TABLE outbox_events (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_type      TEXT        NOT NULL,                 -- e.g. 'OrderConfirmed'
    aggregate_type  TEXT        NOT NULL,                 -- e.g. 'order'
    aggregate_id    BIGINT      NOT NULL,                 -- e.g. the order id
    payload         JSONB       NOT NULL,

    status          TEXT        NOT NULL DEFAULT 'PENDING',
    attempts        INTEGER     NOT NULL DEFAULT 0,
    -- Retry with backoff: a failed event is not picked up again before this.
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at    TIMESTAMPTZ,

    --   PENDING    waiting to be published (possibly after failed attempts)
    --   PROCESSED  published successfully
    --   FAILED     gave up after too many attempts ("dead letter"): needs a human
    CONSTRAINT outbox_events_status_check CHECK (status IN ('PENDING', 'PROCESSED', 'FAILED')),
    CONSTRAINT outbox_events_attempts_check CHECK (attempts >= 0),
    CONSTRAINT outbox_events_processed_at_check CHECK ((status = 'PROCESSED') = (processed_at IS NOT NULL))
);

-- The publisher's main query: "PENDING events that are due, oldest first".
-- Partial: PROCESSED rows (the vast majority over time) are not indexed.
CREATE INDEX outbox_events_pending_due_idx
    ON outbox_events (next_attempt_at, id)
    WHERE status = 'PENDING';

-- Per-aggregate ordering check: "is there an OLDER pending event for the
-- same order?" (see events.Outbox.claimDue).
CREATE INDEX outbox_events_pending_aggregate_idx
    ON outbox_events (aggregate_type, aggregate_id, id)
    WHERE status = 'PENDING';
