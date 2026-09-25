#!/bin/sh
# Smoke test for a built image: starts it with a fresh /config, waits for the healthcheck, checks
# the API (401 without key, first-run setup, 200 with key), the UI, PUID/PGID ownership, and a
# clean SIGTERM stop. HTTP runs inside the container (busybox wget via docker exec) and /config is
# a named volume, so it works against any Docker daemon, including a CI runner's docker-in-docker.
#   sh docker/test-image.sh bunkarr:dev
set -eu
IMAGE=${1:?usage: test-image.sh IMAGE}
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

wget_s "$BASE/" | grep -q '<div id="root">' || fail "web UI not served"
echo "ok: web UI"

owner=$(docker exec "$NAME" stat -c '%u:%g' /config/bunkarr.db)
[ "$owner" = 1234:2345 ] || fail "bunkarr.db owned by $owner, want 1234:2345"
perm=$(docker exec "$NAME" stat -c '%a' /config/bunkarr.key)
[ "$perm" = 600 ] || fail "bunkarr.key mode $perm, want 600"
echo "ok: /config ownership and key permissions"

docker stop -t 20 "$NAME" >/dev/null
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
[ "$exit_code" = 0 ] || fail "exit code after SIGTERM: $exit_code"
docker logs "$NAME" 2>&1 | grep -q 'Stopped' || fail "no clean shutdown in the logs"
echo "ok: clean stop"
echo "PASS"
