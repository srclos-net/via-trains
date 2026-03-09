-- migrations/001_init.sql

CREATE TABLE IF NOT EXISTS transactions (
    id            VARCHAR(36)  PRIMARY KEY,             -- UUID
    booking_ref   VARCHAR(12)  NOT NULL,                -- e.g. VIA-A1B2C3
    amount_cad    NUMERIC(10,2) NOT NULL,
    card_brand    VARCHAR(20)  NOT NULL,
    card_last4    VARCHAR(4)   NOT NULL,
    processor_ref VARCHAR(64)  NOT NULL,                -- "Stripe" charge ID
    status        VARCHAR(20)  NOT NULL DEFAULT 'success',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_transactions_booking_ref ON transactions(booking_ref);
CREATE INDEX IF NOT EXISTS idx_transactions_created_at  ON transactions(created_at DESC);
