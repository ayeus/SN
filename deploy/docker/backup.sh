#!/bin/sh
# Database backups for a single-machine installation (scripts/private.sh).
#
#   backup loop                  keep a dump no older than BACKUP_EVERY_HOURS
#   backup once [label]          take one dump now
#   backup check <file>          restore a dump into a scratch database and report
#   backup replace <file>        replace the live database with a dump
#
# A laptop is asleep at 3 a.m., so `loop` does not run at a fixed hour: it
# looks every few minutes and takes a dump whenever the newest one is too old.
# Every dump is read back before it counts. 14 daily and 8 weekly dumps are
# kept, and the installation's secrets file is copied beside them, because a
# database restored without the keys that signed its contents is of little use.
set -eu

: "${DATABASE_URL:?DATABASE_URL is required}"
DIR="${BACKUP_DIR:-/backups}"
EVERY_HOURS="${BACKUP_EVERY_HOURS:-24}"
KEEP_DAILY="${BACKUP_KEEP_DAILY:-14}"
KEEP_WEEKLY="${BACKUP_KEEP_WEEKLY:-8}"
SECRETS="${SECRETS_FILE:-/secrets/private.env}"
SCRATCH_DB=ayeusann_restore_check

# Errors and warnings only from psql and pg_restore.
export PGOPTIONS="-c client_min_messages=warning"

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) backup: $*"; }

# The same server, another database.
url_for_db() { echo "$DATABASE_URL" | sed "s#/[^/?]*\(?.*\)\{0,1\}\$#/$1\1#"; }

# keep <dir> <n>: delete all but the n newest dumps.
keep() {
    # shellcheck disable=SC2012
    ls -1t "$1"/*.dump 2>/dev/null | tail -n +"$(($2 + 1))" | while read -r old; do
        rm -f "$old"
        log "removed $(basename "$old")"
    done
}

take() {
    label="${1:-daily}"
    mkdir -p "$DIR/$label" "$DIR/weekly"
    stamp=$(date -u +%Y%m%d-%H%M%S)
    partial="$DIR/$label/.partial-$stamp"
    final="$DIR/$label/ayeusann-$stamp.dump"

    if ! pg_dump -Fc --no-owner --no-privileges -f "$partial" "$DATABASE_URL"; then
        rm -f "$partial"
        log "FAILED: pg_dump did not complete"
        return 1
    fi
    # Read it back: a dump that cannot list its own contents, or has no users
    # table in it, is not a backup.
    if ! pg_restore --list "$partial" 2>/dev/null | grep -q "TABLE DATA public users"; then
        rm -f "$partial"
        log "FAILED: the dump could not be read back"
        return 1
    fi
    chmod 600 "$partial"
    mv "$partial" "$final"
    log "wrote $label/$(basename "$final") ($(du -h "$final" | cut -f1))"

    if [ "$label" = daily ]; then
        week="$DIR/weekly/ayeusann-$(date -u +%G-week%V).dump"
        if [ ! -f "$week" ]; then
            cp "$final" "$week"
            chmod 600 "$week"
            log "kept as weekly/$(basename "$week")"
        fi
        keep "$DIR/daily" "$KEEP_DAILY"
        keep "$DIR/weekly" "$KEEP_WEEKLY"
    else
        keep "$DIR/$label" 5
    fi

    if [ -f "$SECRETS" ]; then
        cp "$SECRETS" "$DIR/private.env"
        chmod 600 "$DIR/private.env"
    fi
    date -u +%Y-%m-%dT%H:%M:%SZ > "$DIR/last-backup"
    rm -f "$DIR/last-error"
}

# due: is the newest daily dump older than EVERY_HOURS (or missing)?
due() {
    [ -z "$(find "$DIR/daily" -name '*.dump' -mmin "-$((EVERY_HOURS * 60))" 2>/dev/null | head -1)" ]
}

loop() {
    # As the container's first process this shell gets no default signal
    # handling, so without a trap `docker stop` would wait out its grace period.
    trap 'exit 0' TERM INT
    mkdir -p "$DIR/daily"
    log "watching: a dump whenever the newest is over ${EVERY_HOURS}h old; keeping $KEEP_DAILY daily, $KEEP_WEEKLY weekly"
    while :; do
        if due; then
            take daily || date -u +%Y-%m-%dT%H:%M:%SZ > "$DIR/last-error"
        fi
        sleep "${BACKUP_CHECK_SECONDS:-300}" &
        wait $!
    done
}

report() {
    psql "$1" -At -v ON_ERROR_STOP=1 <<'SQL'
SELECT 'schema version  ' || version || CASE WHEN dirty THEN ' (interrupted migration)' ELSE '' END FROM schema_migrations;
SELECT 'accounts        ' || COUNT(*) FROM users WHERE deleted_at IS NULL;
SELECT 'workspaces      ' || COUNT(*) FROM organizations;
SELECT 'machines        ' || COUNT(*) FROM hosts WHERE deleted_at IS NULL;
SELECT 'deployments     ' || COUNT(*) FROM deployments WHERE deleted_at IS NULL;
SELECT 'API keys        ' || COUNT(*) FROM api_keys;
SELECT 'newest account  ' || COALESCE(to_char(MAX(created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI "UTC"'), 'none') FROM users;
SQL
}

load() { # load <file> <database>
    admin=$(url_for_db postgres)
    psql "$admin" -q -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS $2 WITH (FORCE);" -c "CREATE DATABASE $2;" &&
        pg_restore --no-owner --no-privileges --exit-on-error -d "$(url_for_db "$2")" "$1"
}

drop_scratch() {
    psql "$(url_for_db postgres)" -q -c "DROP DATABASE IF EXISTS $SCRATCH_DB WITH (FORCE);"
}

check() {
    file="$1"
    [ -f "$file" ] || { log "no such dump: $file"; return 1; }
    log "restoring $(basename "$file") into a scratch database (the live one is not touched)"
    if ! load "$file" "$SCRATCH_DB"; then
        drop_scratch
        log "FAILED: this dump does not restore"
        return 1
    fi
    echo
    report "$(url_for_db "$SCRATCH_DB")"
    echo
    drop_scratch
    log "this dump restores cleanly"
}

replace() {
    file="$1"
    [ -f "$file" ] || { log "no such dump: $file"; return 1; }
    live=$(echo "$DATABASE_URL" | sed 's#.*/\([^/?]*\)\(?.*\)\{0,1\}$#\1#')
    # Prove the dump restores before anything is dropped.
    if ! load "$file" "$SCRATCH_DB"; then
        drop_scratch
        log "FAILED: this dump does not restore; the live database was not touched"
        return 1
    fi
    psql "$(url_for_db postgres)" -q -v ON_ERROR_STOP=1 \
        -c "DROP DATABASE IF EXISTS $live WITH (FORCE);" \
        -c "ALTER DATABASE $SCRATCH_DB RENAME TO $live;"
    echo
    report "$DATABASE_URL"
    echo
    log "the database now holds $(basename "$file")"
}

case "${1:-loop}" in
    loop) loop ;;
    once) take "${2:-daily}" ;;
    check) check "${2:?usage: backup check <file>}" ;;
    replace) replace "${2:?usage: backup replace <file>}" ;;
    *) sed -n '2,8p' "$0"; exit 2 ;;
esac
