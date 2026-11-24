#!/bin/sh
set -eu

if [ "$(id -u)" -eq 0 ]; then
    owner=10001:10001
    db=/data/metrics.db
    # Older images created the default database and its sidecars as root.
    chown -h "$owner" /data
    for file in "$db" "$db-wal" "$db-shm" "$db-journal"; do
        if [ -f "$file" ] && [ ! -L "$file" ]; then
            chown -h "$owner" "$file"
        fi
    done
    exec su-exec "$owner" /module "$@"
fi

exec /module "$@"
