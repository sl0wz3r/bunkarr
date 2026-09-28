#!/bin/sh
# Checks deploy/docker-compose.yml with `docker compose config` (nothing is pulled, built or
# started): with every required value set it renders, with the host name bunkarr-<SERVER_NAME>;
# without SERVER_NAME it refuses, like without PUID or TZ, so no install gets a host name every
# other install shares (docs/design/phase4.md §6.7). Values come from a temporary env file, never
# from deploy/.env or the shell. Skipped when the compose plugin is not installed.
#   sh docker/test-compose.sh
set -eu
cd "$(dirname "$0")/.."
if ! docker compose version >/dev/null 2>&1; then
	echo "SKIP: docker compose is not installed"
	exit 0
fi
unset PUID PGID UMASK TZ SERVER_NAME BUNKARR_CONFIG MEDIA_DIR PLEX_DIR BACKUP_DIR \
	SONARR_BACKUPS RADARR_BACKUPS LIDARR_BACKUPS
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# render NAME=VALUE...: docker compose config with exactly these values; output in $dir/out.
render() {
	printf '%s\n' "$@" >"$dir/env"
	docker compose -f deploy/docker-compose.yml --env-file "$dir/env" config >"$dir/out" 2>&1
}

set -- PUID=99 PGID=100 TZ=America/New_York BUNKARR_CONFIG=/srv/bunkarr MEDIA_DIR=/srv/media \
	PLEX_DIR=/srv/plex BACKUP_DIR=/srv/backup
render "$@" SERVER_NAME=testbox || fail "compose config with every required value: $(cat "$dir/out")"
grep -q 'hostname: bunkarr-testbox$' "$dir/out" || fail "host name is not bunkarr-testbox: $(cat "$dir/out")"
echo "ok: renders, host name bunkarr-<SERVER_NAME>"

if render "$@"; then
	fail "compose config without SERVER_NAME rendered: $(grep hostname "$dir/out")"
fi
grep -q 'SERVER_NAME' "$dir/out" || fail "the refusal does not name SERVER_NAME: $(cat "$dir/out")"
echo "ok: refuses without SERVER_NAME"
echo "PASS"
