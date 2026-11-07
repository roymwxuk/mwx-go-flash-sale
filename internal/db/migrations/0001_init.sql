-- ADR-001: initial schema. Every invariant that can live in the database does.

CREATE TABLE drops (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name           TEXT        NOT NULL,
    total_stock    INTEGER     NOT NULL CHECK (total_stock > 0),
    -- Denormalised counter that the stock strategies decrement. It is the hot
    -- row. Conservation (remaining + held + confirmed = total_stock) is
    -- checked against reservations by internal/invariants.
    remaining      INTEGER     NOT NULL CHECK (remaining >= 0),
    per_user_limit INTEGER     NOT NULL DEFAULT 1 CHECK (per_user_limit >= 1),
    price_minor    BIGINT      NOT NULL DEFAULT 1000 CHECK (price_minor >= 0),
    sale_starts_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Used only by strategy D (optimistic concurrency).
    version        BIGINT      NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (remaining <= total_stock),
    CHECK (per_user_limit <= total_stock)
);

CREATE TABLE reservations (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    drop_id    BIGINT      NOT NULL REFERENCES drops (id),
    user_id    TEXT        NOT NULL CHECK (user_id <> ''),
    quantity   INTEGER     NOT NULL CHECK (quantity > 0),
    status     TEXT        NOT NULL DEFAULT 'held'
               CHECK (status IN ('held', 'confirmed', 'expired', 'cancelled')),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Invariant 2 (per-user limit): at most one *active* reservation per user per
-- drop, enforced by the database rather than application code. Expired and
-- cancelled rows fall outside the predicate, so a user can buy again after
-- their hold lapses. quantity <= per_user_limit is checked in the INSERT.
CREATE UNIQUE INDEX reservations_one_active_per_user
    ON reservations (drop_id, user_id)
    WHERE status IN ('held', 'confirmed');

-- Sweeper scan: only held rows, ordered by expiry. Stays small because
-- confirmed/expired rows are not in the index.
CREATE INDEX reservations_held_expiry
    ON reservations (expires_at)
    WHERE status = 'held';

-- Invariant checker / stock state aggregation per drop.
CREATE INDEX reservations_drop_status ON reservations (drop_id, status);

CREATE TABLE idempotency_keys (
    user_id         TEXT        NOT NULL,
    key             TEXT        NOT NULL CHECK (key <> ''),
    endpoint        TEXT        NOT NULL,
    -- SHA-256 over method + path + body. Same key with a different hash -> 422.
    request_hash    BYTEA       NOT NULL,
    -- NULL while the first request is still in flight; set when it completes.
    response_status INTEGER,
    response_body   BYTEA,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    -- Keys are scoped per user, so two users cannot collide on the same key.
    PRIMARY KEY (user_id, key)
);

CREATE INDEX idempotency_keys_created_at ON idempotency_keys (created_at);

CREATE TABLE payments (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Invariant 3 (no double charge): one payment row per reservation, ever.
    reservation_id  BIGINT      NOT NULL UNIQUE REFERENCES reservations (id),
    -- Sent to the (mock) provider. Derived from reservation_id, so every
    -- retry of a confirm presents the same key to the provider.
    idempotency_key TEXT        NOT NULL UNIQUE,
    status          TEXT        NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    amount_minor    BIGINT      NOT NULL CHECK (amount_minor >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
