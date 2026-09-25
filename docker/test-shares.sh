#!/bin/sh
# Network share test (docs/design/phase1.md §9, Docker suite): TestSyncLifecycle and
# TestKillResume run with every destination on a real network share instead of a local directory,
# as Bunkarr is deployed against a NAS: a Samba server mounted over CIFS with Unraid Unassigned
# Devices' options (nounix, noserverino, SMB 3.1.1), and the kernel NFS server mounted over NFSv4.
# The e2e test binary is cross-compiled for the Docker server and runs in a privileged container
# of IMAGE plus cifs-utils and nfs-utils, against the image's own bunkarr binary. The driver is the
# Go test TestDockerShares (internal/e2e, build tag e2e). Needs Go, Docker with privileged
# containers and a kernel with cifs and nfsd (Docker Desktop's has both), and network access for
# apk; removes its containers, volumes, networks and the images it built.
#   sh docker/test-shares.sh bunkarr:dev
#
# SHARES selects the shares (default smb,nfs).
set -eu
IMAGE=${1:?usage: test-shares.sh IMAGE}
cd "$(dirname "$0")/.."
BUNKARR_E2E_IMAGE=$IMAGE BUNKARR_E2E_SHARES=${SHARES:-smb,nfs} \
	exec go test -tags e2e -count=1 -timeout 45m -v -run '^TestDockerShares$' ./internal/e2e/
