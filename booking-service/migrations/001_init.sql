-- migrations/001_init.sql
-- Run once at deploy time (or via an init container)

CREATE TABLE IF NOT EXISTS bookings (
    id           BIGSERIAL PRIMARY KEY,
    reference    VARCHAR(12)  NOT NULL UNIQUE,          -- e.g. VIA-A1B2C3
    origin       VARCHAR(100) NOT NULL,
    destination  VARCHAR(100) NOT NULL,
    departure_at TIMESTAMPTZ  NOT NULL,
    seat_class   VARCHAR(20)  NOT NULL DEFAULT 'economy',
    passenger    VARCHAR(200) NOT NULL DEFAULT 'Guest',
    status       VARCHAR(20)  NOT NULL DEFAULT 'confirmed',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_bookings_reference    ON bookings(reference);
CREATE INDEX IF NOT EXISTS idx_bookings_origin_dest  ON bookings(origin, destination, departure_at);
CREATE INDEX IF NOT EXISTS idx_bookings_created_at   ON bookings(created_at DESC);
