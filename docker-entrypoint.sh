#!/bin/sh
set -eu

if [ "${1#-}" != "$1" ]; then
    set -- /app/qoder2api "$@"
fi

if [ "$1" = "qoder2api" ]; then
    shift
    set -- /app/qoder2api "$@"
fi

mkdir -p /app/auths /app/data /app/data/dsh/profiles/web

if [ ! -f /app/data/dsh/profiles/web/cordis.patch.yml ] && [ -f /app/config/dsh-cordis.patch.example.yml ]; then
    cp /app/config/dsh-cordis.patch.example.yml /app/data/dsh/profiles/web/cordis.patch.yml
fi

if [ "$(id -u)" = "0" ]; then
    chown -R app:app /app/auths /app/data 2>/dev/null || true
    chmod -R 775 /app/auths /app/data 2>/dev/null || true
    exec su-exec app:app "$@"
fi

exec "$@"
