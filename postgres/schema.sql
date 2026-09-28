-- Schema for the postgres implementation.
--
-- Every money column is BIGINT holding money.Money.Scaled()
-- (value * 10^4). Deliberately no DECIMAL/NUMERIC: a scaled integer
-- gives the driver and any ORM zero opportunity to round silently.

DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS ledger;
DROP TABLE IF EXISTS orders;

CREATE TABLE orders (
    id            TEXT PRIMARY KEY,
    amount        BIGINT NOT NULL,
    actual_amount BIGINT NOT NULL DEFAULT 0,
    fee           BIGINT NOT NULL DEFAULT 0,
    status        INT    NOT NULL DEFAULT 0, -- settlement.Status
    settled_at    TIMESTAMPTZ
);

-- Append-only: no UPDATE or DELETE is ever issued against this table.
CREATE TABLE ledger (
    id         BIGSERIAL PRIMARY KEY,
    order_id   TEXT   NOT NULL REFERENCES orders(id),
    amount     BIGINT NOT NULL,
    fee        BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

-- Transactional outbox: rows are written in the same transaction as
-- the settlement that produced them; a relay marks them dispatched
-- after publishing.
CREATE TABLE outbox (
    id            BIGSERIAL PRIMARY KEY,
    event_type    TEXT NOT NULL,
    payload       TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at TIMESTAMPTZ
);

CREATE INDEX idx_outbox_undispatched ON outbox (id) WHERE dispatched_at IS NULL;
