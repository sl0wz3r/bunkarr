# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# Bunkarr container image: multi-arch (linux/amd64, linux/arm64), alpine + tini + su-exec, with
# restic and rclone for the backup engines.
#
#   docker buildx build --platform linux/amd64 --load -t bunkarr:dev .
#   make docker
#
# Dockerfile comments must be on their own lines (a trailing "# ..." after COPY/ARG is parsed as
# arguments). Base images and the frontend are pinned by digest; the tag is kept for readability
# and Renovate. Take the index digest from `docker buildx imagetools inspect <image:tag>`.

# 1. web UI: architecture independent, always built on the build host.
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# 2. Go binary, cross-compiled on the build host (no QEMU for this stage).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.1.0-dev
ARG COMMIT=unknown
ARG BUILD_DATE=
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
# timetzdata embeds the zoneinfo database so TZ works even without the tzdata package.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    pkg=github.com/sl0wz3r/bunkarr/internal/version; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -tags timetzdata \
      -ldflags "-s -w -X ${pkg}.Version=${VERSION} -X ${pkg}.Commit=${COMMIT} -X ${pkg}.BuildDate=${BUILD_DATE}" \
      -o /out/bunkarr ./cmd/bunkarr

# 3. runtime
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG VERSION=0.1.0-dev
ARG COMMIT=unknown
ARG BUILD_DATE=
ARG SOURCE_URL=https://github.com/sl0wz3r/bunkarr

LABEL org.opencontainers.image.title="Bunkarr" \
      org.opencontainers.image.description="Your library's bunker: *arr-style backups for Plex and the *arr stack, with full backups for what is irreplaceable and manifests for what can be re-downloaded." \
      org.opencontainers.image.source="${SOURCE_URL}" \
      org.opencontainers.image.url="${SOURCE_URL}" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

# tini: PID 1. su-exec: drop to PUID:PGID. restic/rclone: backup engines (Phase 4).
# apk upgrade picks up security fixes for the digest-pinned base's own packages.
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates su-exec tini tzdata restic rclone

ENV PUID=1000 \
    PGID=1000 \
    UMASK=002 \
    TZ=Etc/UTC \
    BUNKARR_DOCKER=1 \
    BUNKARR_CONFIG_DIR=/config \
    BUNKARR_PORT=8787

COPY --from=build /out/bunkarr /app/bunkarr
COPY LICENSE /app/LICENSE
COPY --chmod=0755 docker/entrypoint.sh /entrypoint.sh

EXPOSE 8787
VOLUME /config

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --start-interval=5s --retries=3 \
  CMD ["/app/bunkarr", "healthcheck"]

STOPSIGNAL SIGTERM
ENTRYPOINT ["/sbin/tini", "--", "/entrypoint.sh"]
CMD []
