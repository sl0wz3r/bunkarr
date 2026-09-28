#!/bin/sh
# Real-binary engine tests (docs/design/phase4.md D30, §14.4): the tests with build tag enginebin
# of internal/engines/... and internal/enginerun/... run inside golang:1.27-alpine with alpine's
# restic and rclone (docker/engines/Dockerfile), against MinIO (cgr.dev/chainguard/minio) and an
# OpenSSH SFTP server (atmoz/sftp:alpine), both pinned by the digests the engine spike recorded
# (testdata/restic/index.json), on one network. The script makes throwaway credentials, a bucket,
# SFTP host keys (ed25519 and rsa, both pinned) and a passphrase-protected client key, runs
#   go test -tags enginebin -count=1 ./internal/engines/... ./internal/enginerun/...
# with BUNKARR_ENGINEBIN_* set (internal/engines/enginetest/realenv.go), and removes its
# containers, network and test image. It uses docker cp instead of bind mounts, so it works
# against a remote or docker-in-docker daemon too. Needs Docker and network access (image pulls,
# apk, Go modules); the SFTP image is linux/amd64 only (emulated on arm64).
#   sh docker/test-engines.sh        (make test-engines)
#
# ENGINES_PACKAGES replaces the package list; GOTESTFLAGS adds go test flags ("-run X -v").
set -eu
cd "$(dirname "$0")/.."

MINIO_IMAGE=${MINIO_IMAGE:-cgr.dev/chainguard/minio@sha256:6a1d0b45c8669726bba580ced0bfa4cb9fdeed1ed636dfabd81d1577beb6937b}
SFTP_IMAGE=${SFTP_IMAGE:-atmoz/sftp:alpine@sha256:a81ea210713555be76075b4b2788a4addfaa54d137cd881f3a99ac539f0be2c5}
PACKAGES=${ENGINES_PACKAGES:-./internal/engines/... ./internal/enginerun/...}
BUCKET=bunkarr-engines

name=bunkarr-engines-$$
image=$name:test
# shellcheck disable=SC2317,SC2329 # run by the EXIT trap
cleanup() {
	docker rm -f "$name-minio" "$name-sftp" "$name-keys" "$name-test" >/dev/null 2>&1 || true
	docker network rm "$name" >/dev/null 2>&1 || true
	docker image rm "$image" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# Throwaway secrets (never used anywhere else).
random() { od -An -N 16 -tx1 /dev/urandom | tr -d ' \n'; }
s3_key=bk$(random | cut -c1-14)
s3_secret=$(random)
sftp_password=$(random)
key_passphrase=$(random)

echo "== test image"
docker build -q -t "$image" -f docker/engines/Dockerfile . >/dev/null
docker network create "$name" >/dev/null

echo "== MinIO"
docker run -d --name "$name-minio" --network "$name" --network-alias minio \
	-e MINIO_ROOT_USER="$s3_key" -e MINIO_ROOT_PASSWORD="$s3_secret" \
	"$MINIO_IMAGE" server /tmp/minio-data >/dev/null

echo "== SSH keys"
# Made inside a container and copied container to container as tar streams, so the SFTP server's
# files are owned by root (sshd's StrictModes and ChrootDirectory insist) whatever the host is.
docker run --name "$name-keys" --entrypoint sh "$image" -ec '
	mkdir -p /keys/client /out/etc/ssh /out/home/bunkarr/.ssh/keys
	ssh-keygen -q -t ed25519 -N "" -C sftp-host -f /out/etc/ssh/ssh_host_ed25519_key
	ssh-keygen -q -t rsa -b 3072 -N "" -C sftp-host -f /out/etc/ssh/ssh_host_rsa_key
	ssh-keygen -q -t ed25519 -N "$1" -C bunkarr-engines -f /keys/client/id_ed25519
	cat /out/etc/ssh/ssh_host_ed25519_key.pub /out/etc/ssh/ssh_host_rsa_key.pub >/keys/host_keys.pub
	cp /keys/client/id_ed25519.pub /out/home/bunkarr/.ssh/keys/
	chmod 755 /out/etc /out/etc/ssh /out/home /out/home/bunkarr /out/home/bunkarr/.ssh /out/home/bunkarr/.ssh/keys
	chmod 600 /out/etc/ssh/ssh_host_ed25519_key /out/etc/ssh/ssh_host_rsa_key' sh "$key_passphrase"

echo "== SFTP server"
docker create --name "$name-sftp" --platform linux/amd64 --network "$name" --network-alias sftp \
	"$SFTP_IMAGE" "bunkarr:$sftp_password:1001::upload" >/dev/null
for dir in etc home; do
	docker cp "$name-keys:/out/$dir" - | docker cp - "$name-sftp:/"
done
docker start "$name-sftp" >/dev/null

echo "== test container"
docker run -d --name "$name-test" --network "$name" --entrypoint sleep \
	-e BUNKARR_ENGINEBIN_RESTIC=/usr/bin/restic \
	-e BUNKARR_ENGINEBIN_RCLONE=/usr/bin/rclone \
	-e BUNKARR_ENGINEBIN_S3_ENDPOINT=http://minio:9000 \
	-e BUNKARR_ENGINEBIN_S3_KEY_ID="$s3_key" \
	-e BUNKARR_ENGINEBIN_S3_SECRET="$s3_secret" \
	-e BUNKARR_ENGINEBIN_S3_BUCKET="$BUCKET" \
	-e BUNKARR_ENGINEBIN_SFTP_HOST=sftp \
	-e BUNKARR_ENGINEBIN_SFTP_PORT=22 \
	-e BUNKARR_ENGINEBIN_SFTP_USER=bunkarr \
	-e BUNKARR_ENGINEBIN_SFTP_KEY=/keys/client/id_ed25519 \
	-e BUNKARR_ENGINEBIN_SFTP_KEY_PASSPHRASE="$key_passphrase" \
	-e BUNKARR_ENGINEBIN_SFTP_PASSWORD="$sftp_password" \
	-e BUNKARR_ENGINEBIN_SFTP_HOST_KEYS=/keys/host_keys.pub \
	-e BUNKARR_ENGINEBIN_SFTP_PATH=upload \
	"$image" 86400 >/dev/null
docker cp "$name-keys:/keys" - | docker cp - "$name-test:/"
docker rm "$name-keys" >/dev/null
# The module's sources and fixtures only (no web/node_modules, no .git), without macOS extended
# attributes, which the daemon cannot set.
tar_flags=
if tar --version 2>/dev/null | grep -q bsdtar; then
	tar_flags="--no-xattrs --no-mac-metadata"
fi
# shellcheck disable=SC2086 # tar_flags is a word list
COPYFILE_DISABLE=1 tar $tar_flags -cf - go.mod go.sum internal testdata | docker cp - "$name-test:/src"

echo "== waiting for MinIO and the SFTP server"
tries=0
until docker exec -e RCLONE_CONFIG=/dev/null -e RCLONE_CONFIG_SETUP_TYPE=s3 -e RCLONE_CONFIG_SETUP_PROVIDER=Minio \
	-e RCLONE_CONFIG_SETUP_ENDPOINT=http://minio:9000 -e RCLONE_CONFIG_SETUP_ACCESS_KEY_ID="$s3_key" \
	-e RCLONE_CONFIG_SETUP_SECRET_ACCESS_KEY="$s3_secret" "$name-test" rclone mkdir "setup:$BUCKET" 2>/dev/null; do
	tries=$((tries + 1))
	if [ "$tries" -ge 60 ]; then
		docker logs "$name-minio" 2>&1 | tail -20
		echo "MinIO did not start" >&2
		exit 1
	fi
	sleep 1
done
tries=0
until docker logs "$name-sftp" 2>&1 | grep -q 'Server listening'; do
	tries=$((tries + 1))
	if [ "$tries" -ge 90 ]; then
		docker logs "$name-sftp" 2>&1 | tail -20
		echo "the SFTP server did not start" >&2
		exit 1
	fi
	sleep 1
done

# Packages of later slices that are not in this tree yet are skipped.
pkgs=
for p in $PACKAGES; do
	if [ -d "${p%/...}" ]; then
		pkgs="$pkgs $p"
	else
		echo "   (skipping $p: not in this tree)"
	fi
done
echo "== go test -tags enginebin$pkgs"
status=0
# shellcheck disable=SC2086 # pkgs and GOTESTFLAGS are word lists
docker exec -w /src "$name-test" go test -tags enginebin -count=1 -timeout 30m ${GOTESTFLAGS:-} $pkgs || status=$?
if [ "$status" -ne 0 ]; then
	echo "== SFTP server log (last lines)"
	docker logs "$name-sftp" 2>&1 | tail -25
	echo "== MinIO log (last lines)"
	docker logs "$name-minio" 2>&1 | tail -10
fi
exit "$status"
