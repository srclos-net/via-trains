#!/bin/sh
# Runs inside the postgres container at first startup.
# Creates the payments database (bookings is created by POSTGRES_DB env var).
set -e
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    SELECT 'CREATE DATABASE payments'
    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'payments')\gexec
EOSQL
