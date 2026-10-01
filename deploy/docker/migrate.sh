#!/bin/sh
# Applies pending migrations, then the seed files (catalogue and rate card).
# Seeds are idempotent, so this runs on every deploy.
set -eu
: "${DATABASE_URL:?DATABASE_URL is required}"

migrate -path /app/schema/migrations -database "$DATABASE_URL" up
for f in /app/schema/seeds/*.sql; do
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$f"
done
echo "database migrated and seeded"
