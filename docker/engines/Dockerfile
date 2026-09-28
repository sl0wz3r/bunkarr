# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# Test image of make test-engines (docker/test-engines.sh; docs/design/phase4.md D30, §14.4): Go
# plus alpine's restic and rclone (the versions the Bunkarr image ships: its base is alpine 3.24
# too) and ssh-keygen for the throwaway SFTP keys. The base is the golang image the main
# Dockerfile builds with, pinned by the same digest. Only go.mod, go.sum and
# docker/engines/versions.env are in the build context (Dockerfile.dockerignore), so the module
# download is cached; the script copies the source in with docker cp.
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414
# The same upstream versions as the Bunkarr image (docker/engines/versions.env).
RUN --mount=type=bind,source=docker/engines/versions.env,target=/run/bunkarr-engine-versions \
    . /run/bunkarr-engine-versions && \
    apk add --no-cache "restic~${RESTIC_VERSION}" "rclone~${RCLONE_VERSION}" openssh-keygen
# No C compiler in the image: the real-binary tests run without -race.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
