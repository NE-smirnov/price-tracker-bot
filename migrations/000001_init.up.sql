-- Users of the bot. Identified by Telegram account, not by our own login.
CREATE TABLE users (
    id                UUID        PRIMARY KEY,
    telegram_id       BIGINT      NOT NULL UNIQUE,
    username          TEXT        NOT NULL DEFAULT '',
    language_code     TEXT        NOT NULL DEFAULT '',
    default_currency  CHAR(3)     NOT NULL DEFAULT 'USD'
        CONSTRAINT users_default_currency_iso CHECK (default_currency ~ '^[A-Z]{3}$'),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Products a user watches.
--
-- Money is stored as an exact integer amount of minor units plus its currency
-- code; there is deliberately no NUMERIC or DOUBLE column for prices.
CREATE TABLE tracked_items (
    id                     UUID        PRIMARY KEY,
    user_id                UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    url                    TEXT        NOT NULL,
    title                  TEXT        NOT NULL DEFAULT '',

    -- Alert threshold. Both columns are set together or both are NULL.
    target_amount          BIGINT,
    target_currency        CHAR(3)
        CONSTRAINT tracked_items_target_currency_iso
            CHECK (target_currency IS NULL OR target_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT tracked_items_target_price_complete CHECK (
        (target_amount IS NULL AND target_currency IS NULL)
        OR (target_amount IS NOT NULL AND target_currency IS NOT NULL AND target_amount > 0)
    ),

    check_interval_seconds INTEGER     NOT NULL
        CONSTRAINT tracked_items_interval_range
            CHECK (check_interval_seconds BETWEEN 300 AND 86400),
    active                 BOOLEAN     NOT NULL DEFAULT TRUE,

    -- Scheduling state. next_check_at drives the scraper; claimed_until is the
    -- lease that stops two workers from fetching the same page at once.
    next_check_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_until          TIMESTAMPTZ,

    -- Consecutive scrape failures and the last error, used to warn the user
    -- that a parser has broken rather than silently going quiet.
    failure_streak         INTEGER     NOT NULL DEFAULT 0,
    last_error             TEXT        NOT NULL DEFAULT '',

    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One URL per user: adding the same product twice is a user mistake.
    CONSTRAINT tracked_items_user_url_unique UNIQUE (user_id, url)
);

CREATE INDEX tracked_items_user_idx ON tracked_items (user_id);

-- Partial index: the scheduler only ever scans active, unleased, due rows, and
-- this keeps that scan independent of how much inactive history accumulates.
CREATE INDEX tracked_items_due_idx
    ON tracked_items (next_check_at)
    WHERE active;

-- One observation of a product's price and availability.
CREATE TABLE price_snapshots (
    id                  UUID        PRIMARY KEY,
    tracked_item_id     UUID        NOT NULL REFERENCES tracked_items (id) ON DELETE CASCADE,

    -- Price exactly as shown by the shop.
    amount              BIGINT      NOT NULL CONSTRAINT price_snapshots_amount_nonneg CHECK (amount >= 0),
    currency            CHAR(3)     NOT NULL
        CONSTRAINT price_snapshots_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),

    -- Same amount in the user's currency, when a rate was available. Nullable
    -- on purpose: a missing exchange rate must not lose the original price.
    converted_amount    BIGINT
        CONSTRAINT price_snapshots_converted_nonneg CHECK (converted_amount IS NULL OR converted_amount >= 0),
    converted_currency  CHAR(3)
        CONSTRAINT price_snapshots_converted_currency_iso
            CHECK (converted_currency IS NULL OR converted_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT price_snapshots_converted_complete CHECK (
        (converted_amount IS NULL AND converted_currency IS NULL)
        OR (converted_amount IS NOT NULL AND converted_currency IS NOT NULL)
    ),

    in_stock            BOOLEAN     NOT NULL,
    observed_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every history and stats query is "latest N for this item", so the index is
-- ordered to serve it directly.
CREATE INDEX price_snapshots_item_observed_idx
    ON price_snapshots (tracked_item_id, observed_at DESC);

-- Alerts that were produced for an item. Kept in the database rather than only
-- in the queue so that at-least-once delivery can be deduplicated and so the
-- user can be shown what was already reported.
CREATE TABLE alerts (
    id               UUID        PRIMARY KEY,
    tracked_item_id  UUID        NOT NULL REFERENCES tracked_items (id) ON DELETE CASCADE,
    user_id          UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind             TEXT        NOT NULL
        CONSTRAINT alerts_kind_known CHECK (
            kind IN ('price_drop', 'back_in_stock', 'out_of_stock', 'all_time_low', 'scrape_degraded')
        ),

    -- Nullable on purpose: a 'scrape_degraded' alert is about the parser, not
    -- about a price, and inventing a zero amount for it would be a lie.
    amount           BIGINT,
    currency         CHAR(3)
        CONSTRAINT alerts_currency_iso CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    CONSTRAINT alerts_price_complete CHECK (
        (amount IS NULL AND currency IS NULL) OR (amount IS NOT NULL AND currency IS NOT NULL)
    ),
    previous_amount  BIGINT,
    previous_currency CHAR(3)
        CONSTRAINT alerts_previous_currency_iso
            CHECK (previous_currency IS NULL OR previous_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT alerts_previous_price_complete CHECK (
        (previous_amount IS NULL AND previous_currency IS NULL)
        OR (previous_amount IS NOT NULL AND previous_currency IS NOT NULL)
    ),

    -- Stable identity of "this alert about this item at this price". The unique
    -- index is what makes a retried delivery a no-op instead of a duplicate
    -- message to the user.
    dedup_key        TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at     TIMESTAMPTZ,

    CONSTRAINT alerts_dedup_key_unique UNIQUE (dedup_key)
);

CREATE INDEX alerts_item_created_idx ON alerts (tracked_item_id, created_at DESC);
