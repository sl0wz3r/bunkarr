#!/bin/sh
# Smoke test for a built image: starts it with a fresh /config, waits for the healthcheck, checks
# the API (401 without key, first-run setup, 200 with key), restic and rclone (present, at the
# versions of docker/engines/versions.env, restic runs), the UI, PUID/PGID ownership, a clean
# SIGTERM stop, and a restart with other PUID/PGID on the same /config. HTTP runs inside the
# container (busybox wget via docker exec) and /config is a named volume, so it works against any
# Docker daemon, including a CI runner's docker-in-docker.
#   sh docker/test-image.sh bunkarr:dev
set -eu
IMAGE=${1:?usage: test-image.sh IMAGE}
# RESTIC_VERSION, RCLONE_VERSION: what the image must ship.
# shellcheck disable=SC1091 # docker/engines/versions.env, next to this script
. "$(dirname "$0")/engines/versions.env"
NAME=bunkarr-test-$$
VOL=bunkarr-test-config-$$
BASE=http://127.0.0.1:8787

cleanup() {
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	docker volume rm "$VOL" >/dev/null 2>&1 || true
}
trap cleanup EXIT
fail() {
	echo "FAIL: $*" >&2
	docker logs "$NAME" >&2 || true
	exit 1
}

# wget_s ARGS...: wget inside the container with server headers (-S) on stdout; never fails.
wget_s() {
	docker exec "$NAME" wget -S -q -O - "$@" 2>&1 || true
}
# status_of OUTPUT: the last HTTP status code in wget -S output.
status_of() {
	printf '%s\n' "$1" | awk '$1 ~ /^HTTP\// {code = $2} END {print code}'
}

docker volume create "$VOL" >/dev/null
docker run -d --name "$NAME" -e PUID=1234 -e PGID=2345 -v "$VOL:/config" "$IMAGE" >/dev/null

i=0
until [ "$(docker inspect -f '{{.State.Health.Status}}' "$NAME")" = healthy ]; do
	i=$((i + 1))
	[ "$i" -le 60 ] || fail "container did not become healthy"
	sleep 1
done
echo "ok: healthy"

out=$(wget_s "$BASE/api/v1/system/status")
[ "$(status_of "$out")" = 401 ] || fail "status without API key: $out"
echo "ok: 401 without API key"

out=$(wget_s --header 'Content-Type: application/json' \
	--post-data '{"username":"smoke","password":"smoke-test-pw"}' "$BASE/api/v1/auth/setup")
[ "$(status_of "$out")" = 201 ] || fail "first-run setup: $out"
token=$(printf '%s\n' "$out" | sed -n 's/.*Set-Cookie: bunkarr_session=\([^;]*\);.*/\1/p' | head -n 1)
[ -n "$token" ] || fail "no session cookie after setup: $out"
key=$(wget_s --header "Cookie: bunkarr_session=$token" "$BASE/api/v1/settings/general" |
	sed -n 's/.*"apiKey":"\([0-9a-f]*\)".*/\1/p')
[ -n "$key" ] || fail "could not read the API key after setup"
out=$(wget_s --header "X-Api-Key: $key" "$BASE/api/v1/system/status")
printf '%s' "$out" | grep -q '"databasePath":"/config/bunkarr.db"' || fail "status with API key: $out"
echo "ok: setup, then status with API key"

# The engines as start-up found them (docs/design/phase4.md §10.1): available, and the upstream
# versions the tests measured (alpine's build suffix, e.g. rclone's "-DEV", is ignored).
for engine in restic rclone; do
	want=$RESTIC_VERSION
	[ "$engine" = restic ] || want=$RCLONE_VERSION
	got=$(printf '%s' "$out" | sed -n "s/.*\"$engine\":{\"available\":true,\"version\":\"\([^\"]*\)\".*/\1/p")
	[ -n "$got" ] || fail "$engine is not available in the image: $out"
	[ "${got%%-*}" = "$want" ] || fail "$engine $got in the image, want $want (docker/engines/versions.env)"
done
echo "ok: restic $RESTIC_VERSION and rclone $RCLONE_VERSION available"

# engine_test: a restic command through Bunkarr (the test of a local repository, which runs in a
# run directory under /config/run); the target belongs to $1.
engine_test() {
	docker exec "$NAME" sh -c 'mkdir -p /tmp/restic-test && chown "$1" /tmp/restic-test' sh "$1"
	wget_s --header "X-Api-Key: $key" --header 'Content-Type: application/json' \
		--post-data '{"kind":"local","engine":"restic","target":"/tmp/restic-test"}' "$BASE/api/v1/destinations/test"
}
out=$(engine_test 1234:2345)
printf '%s' "$out" | grep -q '"ok":true' || fail "restic test of a local repository: $out"
echo "ok: restic runs"

wget_s "$BASE/" | grep -q '<div id="root">' || fail "web UI not served"
echo "ok: web UI"

owner=$(docker exec "$NAME" stat -c '%u:%g' /config/bunkarr.db)
[ "$owner" = 1234:2345 ] || fail "bunkarr.db owned by $owner, want 1234:2345"
perm=$(docker exec "$NAME" stat -c '%a' /config/bunkarr.key)
[ "$perm" = 600 ] || fail "bunkarr.key mode $perm, want 600"
echo "ok: /config ownership and key permissions"

# What earlier runs keep in /config, as the old user and 0700 like Bunkarr and restic make them:
# a run directory a killed command left, a restic cache, a config-version staging directory and
# the pre-migration database copies (backups: a later upgrade's migration writes a new one there).
docker exec -u 1234:2345 "$NAME" sh -ec 'umask 077
	for d in run/7-0123456789abcdef-data cache/restic/1/data staging/versions/1 backups; do
		mkdir -p "/config/$d" && echo x >"/config/$d/f"
	done'

docker stop -t 20 "$NAME" >/dev/null
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
[ "$exit_code" = 0 ] || fail "exit code after SIGTERM: $exit_code"
docker logs "$NAME" 2>&1 | grep -q 'Stopped' || fail "no clean shutdown in the logs"
echo "ok: clean stop"

# Other PUID/PGID on the same /config (e.g. the image default 1000 first, then Unraid's 99:100):
# the entrypoint gives Bunkarr's files and its 0700 directories to the new ids, so the start-up
# sweep empties /config/run and restic runs again.
docker rm "$NAME" >/dev/null
docker run -d --name "$NAME" -e PUID=4321 -e PGID=5432 -v "$VOL:/config" "$IMAGE" >/dev/null
i=0
until [ "$(docker inspect -f '{{.State.Health.Status}}' "$NAME")" = healthy ]; do
	i=$((i + 1))
	[ "$i" -le 60 ] || fail "container did not become healthy after the PUID/PGID change"
	sleep 1
done
for p in /config/run /config/cache /config/cache/restic/1/data/f /config/staging /config/staging/versions/1/f \
	/config/backups /config/backups/f; do
	owner=$(docker exec "$NAME" stat -c '%u:%g' "$p") || fail "$p is missing after the PUID/PGID change"
	[ "$owner" = 4321:5432 ] || fail "$p owned by $owner after the PUID/PGID change, want 4321:5432"
done
[ -z "$(docker exec "$NAME" ls -A /config/run)" ] || fail "the start-up sweep left $(docker exec "$NAME" ls -A /config/run) in /config/run"
if docker logs "$NAME" 2>&1 | grep -q 'Could not remove the run directories'; then
	fail "start-up could not sweep /config/run after the PUID/PGID change"
fi
out=$(engine_test 4321:5432)
printf '%s' "$out" | grep -q '"ok":true' || fail "restic test after the PUID/PGID change: $out"
echo "ok: PUID/PGID change: run, cache, staging and backups re-owned, restic runs"
echo "PASS"
