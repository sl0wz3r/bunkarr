#!/bin/sh
# *arr acceptance test (docs/design/phase2-3.md §15 Docker suite items 1 and 2, and acceptance 4
# through the *arrs' own webhook tests): real Radarr and Sonarr containers of the fixture spike's
# versions import small real MKVs into a media volume the Bunkarr image reads, each posting its
# webhooks to Bunkarr with the integration's webhook key; an import must be at the destination
# within 60 s, copied alone by a targeted sync with trigger webhook, and an upgrade must copy the
# new file before it retains the old one (also when the new file is copied from another volume,
# and as an update when it lands at the same path). The driver is the Go test TestDockerArr
# (internal/e2e, build tag e2e). Needs Go, Docker and internet (the *arrs' metadata lookups and
# alpine's ffmpeg package; without it the test skips). Removes its containers, volumes, network
# and the ffmpeg image it builds; keeps the pulled images.
#   sh docker/test-arr.sh bunkarr:dev
#
# The same run covers Lidarr (TestDockerArrLidarr), the manifest round trip, and the *arr backup
# tests of internal/arrbackup (acceptance 5: Radarr and Lidarr with their Backups folder mounted
# read-only, login required, a reused scheduled backup).
#
# SONARR_IMAGE, RADARR_IMAGE and LIDARR_IMAGE override the images: linuxserver/sonarr 4.0.20.3014,
# linuxserver/radarr 6.4.4.10685 and linuxserver/lidarr 3.1.0.4875-ls42, pinned by index digest
# (the fixture spike's versions).
set -eu
IMAGE=${1:?usage: test-arr.sh IMAGE}
SONARR_IMAGE=${SONARR_IMAGE:-lscr.io/linuxserver/sonarr@sha256:a5c1a5fecbef946927ab90ad68df319ac5fe644057e5fc18cd993f01ac07b2b2}
RADARR_IMAGE=${RADARR_IMAGE:-lscr.io/linuxserver/radarr@sha256:adb6c09d6b729ea5e642c99cea35af72702ef476bf4763f153299ac5db9f0b4f}
LIDARR_IMAGE=${LIDARR_IMAGE:-lscr.io/linuxserver/lidarr@sha256:044d616beb43c5e7810991242c6a9c42b93ff238c8c0850684939634ea751208}
cd "$(dirname "$0")/.."
export BUNKARR_E2E_IMAGE="$IMAGE" BUNKARR_E2E_ARR=1 BUNKARR_E2E_SONARR_IMAGE="$SONARR_IMAGE" \
	BUNKARR_E2E_RADARR_IMAGE="$RADARR_IMAGE" BUNKARR_E2E_LIDARR_IMAGE="$LIDARR_IMAGE"
go test -tags e2e -count=1 -timeout 40m -v -run '^TestDockerArr' ./internal/e2e/
exec go test -count=1 -timeout 30m -v -run '^TestDockerArrBackup' ./internal/arrbackup/
