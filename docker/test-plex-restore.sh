#!/bin/sh
# Plex restore test (acceptance 4; docs/design/phase1.md §9, ADR 0005, spike 0001): a scratch
# Plex Media Server with a Movies library over generated fake files runs under a scrobble/refresh
# write load; the Bunkarr image backs its database up from a read-only mount of Plex's config;
# every version must pass Bunkarr's verification and `Plex SQLite ... "PRAGMA integrity_check"`,
# and the newest one is restored into a second, fresh Plex container that must serve the same
# sections, movie count and watched count. Before the backups, Bunkarr lists the Plex sections with
# the Movies location mapped into its own mount (/data -> /media), imports it as a source and syncs
# it to a second destination. The driver is the Go test TestDockerPlexRestore
# (internal/e2e, build tag e2e). Slow (two PMS containers) and pulls Plex on first use. Needs Go
# and Docker; removes its containers, volumes and network, keeps the images.
#   sh docker/test-plex-restore.sh bunkarr:dev
#
# PLEX_IMAGE overrides the Plex image. The default is plexinc/pms-docker pinned by the index
# digest recorded in the spike (PMS 1.43.4.10903, Plex SQLite 3.53.3; linux/amd64, arm64, arm/v7).
set -eu
IMAGE=${1:?usage: test-plex-restore.sh IMAGE}
PLEX_IMAGE=${PLEX_IMAGE:-plexinc/pms-docker@sha256:e0ab27395614a8e1a4fdf84c6bc60ac664915cfdde70c52d030c7728a1c48e14}
cd "$(dirname "$0")/.."
BUNKARR_E2E_IMAGE=$IMAGE BUNKARR_E2E_PLEX_IMAGE=$PLEX_IMAGE \
	exec go test -tags e2e -count=1 -timeout 30m -v -run '^TestDockerPlexRestore$' ./internal/e2e/
