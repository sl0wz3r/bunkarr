#!/bin/sh
# Container kill test (docs/design/phase1.md §9, Docker suite): the image runs with
# BUNKARR_FAULTPOINT=copy.afterWrite, a sync parks after writing its first temp file, the
# container is killed with `docker kill -s KILL` and started again with `docker start`; the job
# must resume (attempt 2) and complete, a full verify must pass and the backup volume must hold
# exactly the source's files and no temp file. The driver is the Go test TestDockerKillResume
# (internal/e2e, build tag e2e); it uses named volumes and `docker exec`, so it also works against
# a CI runner's docker-in-docker daemon. Needs Go and Docker; removes its containers and volumes.
#   sh docker/test-kill.sh bunkarr:dev
set -eu
IMAGE=${1:?usage: test-kill.sh IMAGE}
cd "$(dirname "$0")/.."
BUNKARR_E2E_IMAGE=$IMAGE exec go test -tags e2e -count=1 -timeout 20m -v -run '^TestDockerKillResume$' ./internal/e2e/
