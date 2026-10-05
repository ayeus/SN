#!/bin/sh
# Applies pending migrations, then the seed files (catalogue and rate card).
# Seeds are idempotent, so this runs on every deploy.
#
# With BACKUP_BEFORE_MIGRATE=1 (a single-machine installation), a database that
# already holds data is dumped before any pending migration touches it.
set -eu
: "${DATABASE_URL:?DATABASE_URL is required}"

if [ "${BACKUP_BEFORE_MIGRATE:-0}" = 1 ]; then
    have=$(psql "$DATABASE_URL" -Atc "SELECT version FROM schema_migrations LIMIT 1" 2>/dev/null || true)
    want=$(ls /app/schema/migrations/*.up.sql | sed 's#.*/0*\([0-9][0-9]*\)_.*#\1#' | sort -n | tail -1)
    if [ -n "$have" ] && [ "$have" != "$want" ]; then
        echo "schema is at version $have and will move to $want: taking a dump first"
        backup once before-upgrade
    fi
fi

migrate -path /app/schema/migrations -database "$DATABASE_URL" up
for f in /app/schema/seeds/*.sql; do
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$f"
done
echo "database migrated and seeded"
