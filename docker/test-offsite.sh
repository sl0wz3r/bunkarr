#!/bin/sh
# Off-site acceptance suite (docs/design/phase4.md §14.6, acceptance 1-9): the Bunkarr image with
# restic and rclone destinations on MinIO (cgr.dev/chainguard/minio) and an OpenSSH SFTP server
# (atmoz/sftp:alpine), both pinned by the digests the engine spike recorded
# (testdata/restic/index.json, as make test-engines), on one network; the UNAS stand-in is a named
# volume (allowLocal). The drivers are the Go tests TestDockerOffsite* (internal/e2e, build tag
# e2e); they build a derived test image FROM the image under test with
#   - an e2e build of this checkout (go build -tags e2e ./cmd/bunkarr, cross-compiled for the
#     image's platform) over /app/bunkarr, so BUNKARR_TEST_WINDOW works (acceptance 5);
#   - the argv shims of docker/offsite/argv-shim.sh as /opt/bunkarr-e2e/bin/{restic,rclone}
#     (root-owned; BUNKARR_RESTIC_PATH and BUNKARR_RCLONE_PATH point at them, acceptance 4);
#   - curl, for API calls with PUT and DELETE and with bodies kept off argv;
# and drive everything over HTTP through docker exec, with named volumes and docker cp, so they
# also work against a remote or docker-in-docker daemon. The recovery-kit test restores in a fresh
# golang:1.27-alpine container (apk add restic rclone). Needs Go, Docker and network access (apk,
# image pulls); the SFTP image is linux/amd64 only (emulated on arm64). Removes its containers,
# volumes, network and built images; pulled images are kept.
#   sh docker/test-offsite.sh bunkarr:dev        (make test-offsite)
#
# GOTESTFLAGS adds go test flags ("-run TestDockerOffsiteKit"). MINIO_IMAGE, SFTP_IMAGE and
# GOLANG_IMAGE override the pinned images. BUNKARR_E2E_OFFSITE_HOLD=15m keeps a failed test's
# containers that long for inspection (docker exec into them; the argv log is /argvlog/argv.log).
# The manual real-B2 test (TestDockerOffsiteB2) runs only with BUNKARR_E2E_B2_KEY_ID,
# BUNKARR_E2E_B2_KEY and BUNKARR_E2E_B2_BUCKET set, never in CI.
set -eu
IMAGE=${1:?usage: test-offsite.sh IMAGE}
cd "$(dirname "$0")/.."
BUNKARR_E2E_MINIO_IMAGE=${MINIO_IMAGE:-cgr.dev/chainguard/minio@sha256:6a1d0b45c8669726bba580ced0bfa4cb9fdeed1ed636dfabd81d1577beb6937b}
BUNKARR_E2E_SFTP_IMAGE=${SFTP_IMAGE:-atmoz/sftp:alpine@sha256:a81ea210713555be76075b4b2788a4addfaa54d137cd881f3a99ac539f0be2c5}
BUNKARR_E2E_GOLANG_IMAGE=${GOLANG_IMAGE:-golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414}
export BUNKARR_E2E_MINIO_IMAGE BUNKARR_E2E_SFTP_IMAGE BUNKARR_E2E_GOLANG_IMAGE
# GOTESTFLAGS comes last, so its -run narrows the default one.
# shellcheck disable=SC2086 # GOTESTFLAGS is a word list
BUNKARR_E2E_IMAGE=$IMAGE BUNKARR_E2E_OFFSITE=1 \
	exec go test -tags e2e -count=1 -timeout 120m -v -run '^TestDockerOffsite' ${GOTESTFLAGS:-} ./internal/e2e/
