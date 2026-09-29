#!/bin/sh
# Compares the Go release the image's binary was built with (the digest-pinned golang image in the
# Dockerfile) with the Go toolchain on PATH (in CI: actions/setup-go with check-latest, i.e. the
# latest patch release of the same Go version). The standard library is compiled into the binary,
# so its security fixes only reach the image when the Dockerfile's golang digest is bumped.
#
#   sh docker/check-go-version.sh IMAGE          exit 1 when the image was built with an older Go
#   sh docker/check-go-version.sh --warn IMAGE   only print a warning (branch CI)
set -eu

MODE=fail
if [ "${1:-}" = --warn ]; then
	MODE=warn
	shift
fi
IMAGE=${1:?usage: check-go-version.sh [--warn] IMAGE}

TMP=$(mktemp -d)
CID=""
# shellcheck disable=SC2317,SC2329 # run by the EXIT trap
cleanup() {
	if [ -n "$CID" ]; then
		docker rm -f -v "$CID" >/dev/null 2>&1 || true
	fi
	rm -rf "$TMP"
}
trap cleanup EXIT

# -v on removal: the image declares VOLUME /config, so the container gets an anonymous volume.
# Nothing runs: docker create only makes the container that docker cp reads the binary from.
CID=$(docker create "$IMAGE")
docker cp "$CID:/app/bunkarr" "$TMP/bunkarr" >/dev/null
BUILT=$(go version "$TMP/bunkarr" | awk '{print $NF}')
LATEST=$(go env GOVERSION)
echo "$IMAGE: binary built with $BUILT; latest toolchain: $LATEST"

case $BUILT in
go[0-9]*) ;;
*)
	echo "could not read the Go version of $IMAGE:/app/bunkarr" >&2
	exit 1
	;;
esac
if [ "$BUILT" = "$LATEST" ] || [ "$(printf '%s\n%s\n' "$BUILT" "$LATEST" | sort -V | head -n 1)" = "$LATEST" ]; then
	exit 0
fi

MSG="$IMAGE was built with $BUILT, but $LATEST is out: the binary misses the standard library's newer (security) fixes. Update the golang image digest in the Dockerfile (docker buildx imagetools inspect golang:<version>-alpine)."
if [ "$MODE" = warn ]; then
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::warning::$MSG"
	else
		echo "WARNING: $MSG" >&2
	fi
	exit 0
fi
echo "ERROR: $MSG" >&2
exit 1
